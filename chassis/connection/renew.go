package connection

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Renewal (docs/todo-connections.md §7.1). A token renews when it is used
// and its margin has begun. One node claims the row and renews; the others
// keep using the current token while it is valid. Every write is fenced by
// the claim and the generation. An expired claim means "uncertain", never
// "go ahead": what follows is the provider's declared rule.

const (
	// claimLease is how long a claimant has. Longer than RequestTimeout, so
	// a claimant still waiting on the provider has not lost its claim.
	claimLease = 60 * time.Second
	// busyWait is how long a use whose token has expired waits for the
	// claimant's new generation before answering busy.
	busyWait = 5 * time.Second
	busyPoll = 200 * time.Millisecond
)

// margin is when renewal starts before expiry: a fraction of the lifetime,
// clamped, and never more than half of it — a 30-second token renews in
// its last 15 seconds, an 8-hour one in its last 15 minutes.
func margin(p *Provider, lifetimeS int64) time.Duration {
	if lifetimeS <= 0 {
		return 0
	}
	life := time.Duration(lifetimeS) * time.Second
	m := time.Duration(p.Refresh.Margin.Fraction * float64(life))
	if floor := time.Duration(p.Refresh.Margin.FloorS) * time.Second; m < floor {
		m = floor
	}
	if ceil := time.Duration(p.Refresh.Margin.CeilingS) * time.Second; m > ceil {
		m = ceil
	}
	if m > life/2 {
		m = life / 2
	}
	return m
}

func (s *Service) reload(ctx context.Context, row Row) Row {
	if r, err := s.store.Reload(ctx, row.ID); err == nil {
		return r
	}
	return row
}

// current returns the access token to use now, renewing first when due.
func (s *Service) current(ctx context.Context, tenant, declStack string, p *Provider, row Row) (string, Row, *Error) {
	now := s.now()
	// A claim past its lease: its holder may have died after sending the
	// refresh token. Nobody takes it over; the row is uncertain.
	if row.ClaimID != "" && row.ClaimUntil != nil && row.ClaimUntil.Before(now) {
		if ok, _ := s.store.ReleaseStale(ctx, row); ok {
			s.event(ctx, "connection.uncertain", map[string]any{"principal": row.PrincipalID, "provider": row.Provider, "why": "a renewal's claim expired"})
		}
		row = s.reload(ctx, row)
	}
	if row.Status == StatusReconnect {
		return "", row, fail(CodeReconnect, "%s refused the %s connection's refresh token: the person reconnects it", providerTitle(p, row.Provider), row.Provider)
	}
	expired := row.ExpiresAt != nil && !now.Before(*row.ExpiresAt)
	due := row.ExpiresAt != nil && !now.Before(row.ExpiresAt.Add(-margin(p, row.LifetimeS)))
	m, err := s.store.Open(ctx, row)
	if err != nil {
		return "", row, fail(CodeUnavailable, "the stored connection could not be opened: %v", err)
	}
	mayRenew := m.Refresh != "" && (row.Status == StatusActive || (row.Status == StatusUncertain && p.Refresh.OnUncertain == OnUncertainRetry))
	if due && mayRenew {
		if row.ClaimID == "" {
			mine := "clm_" + hxid.NewTimeSort().String()
			claimed, cerr := s.store.Claim(ctx, row.ID, row.Generation, mine, now.Add(claimLease), row.Status)
			if cerr != nil {
				return "", row, fail(CodeStore, "%v", cerr)
			}
			if claimed {
				if expired {
					return s.renewNow(ctx, tenant, declStack, p, row, mine, m)
				}
				s.renewLater(ctx, tenant, declStack, p, row, mine, m)
				return m.Access, row, nil
			}
			row = s.reload(ctx, row)
		}
		// Another node holds the claim.
		if !expired {
			return m.Access, row, nil
		}
		return s.waitRenewed(ctx, p, row)
	}
	if expired {
		switch {
		case row.Status == StatusUncertain:
			return "", row, fail(CodeReconnect, "a renewal of the %s connection had an unknown outcome and its token has expired: the person reconnects it", row.Provider)
		case m.Refresh == "":
			return "", row, fail(CodeReconnect, "the %s connection's token has expired and it has no refresh token: the person reconnects it", row.Provider)
		}
		// Due, a refresh token, but a claim is live: wait for it.
		return s.waitRenewed(ctx, p, row)
	}
	return m.Access, row, nil
}

// renewNow renews under a claim this caller holds, and returns the token
// to use.
func (s *Service) renewNow(ctx context.Context, tenant, declStack string, p *Provider, row Row, claimID string, m Material) (string, Row, *Error) {
	now := s.now()
	expired := row.ExpiresAt != nil && !now.Before(*row.ExpiresAt)
	creds, cerr := s.creds(ctx, tenant, declStack, p)
	if cerr != nil {
		_ = s.store.Release(ctx, row, claimID)
		return "", row, cerr
	}
	a, out, err := s.refresh(ctx, p, creds, m.Refresh)
	fields := map[string]any{"principal": row.PrincipalID, "provider": row.Provider, "generation": row.Generation}
	switch out {
	case outcomeOK:
		next := Material{Access: a.Access, Refresh: a.Refresh, TokenType: a.TokenType}
		if next.Refresh == "" && p.Refresh.Rotation == RotationReusable {
			next.Refresh = m.Refresh
		}
		if next.TokenType == "" {
			next.TokenType = m.TokenType
		}
		expires, lifetime := expiry(now, a.ExpiresIn)
		refreshExpires, _ := expiry(now, a.RefreshExpiresIn)
		if refreshExpires == nil && next.Refresh == m.Refresh {
			refreshExpires = row.RefreshExpiresAt
		}
		written, werr := s.store.Renew(ctx, row, claimID, next, expires, refreshExpires, lifetime)
		if errors.Is(werr, ErrFenced) {
			// The row moved on without us (the claim was cleared as stale and
			// another renewal won): use what is there now.
			fields["fenced"] = true
			s.event(ctx, "connection.renewed", fields)
			latest := s.reload(ctx, row)
			lm, oerr := s.store.Open(ctx, latest)
			if oerr != nil {
				return "", latest, fail(CodeUnavailable, "%v", oerr)
			}
			return lm.Access, latest, nil
		}
		if werr != nil {
			return "", row, fail(CodeStore, "%v", werr)
		}
		fields["generation"] = written.Generation
		s.event(ctx, "connection.renewed", fields)
		return a.Access, written, nil
	case outcomeRefused:
		fields["error"] = a.Err
		if serr := s.store.SetStatus(ctx, row, claimID, StatusReconnect); serr != nil {
			s.log.Warn("connection: reconnect verdict fenced", zap.Error(serr))
		}
		s.event(ctx, "connection.reconnect", fields)
		return "", row, fail(CodeReconnect, "%s refused the %s connection's refresh token: the person reconnects it", providerTitle(p, row.Provider), row.Provider)
	case outcomeNotSent:
		_ = s.store.Release(ctx, row, claimID)
		if !expired {
			return m.Access, row, nil
		}
		return "", row, fail(CodeUnavailable, "%s could not be reached to renew the token: %v", providerTitle(p, row.Provider), err)
	default: // outcomeUncertain
		fields["why"] = err.Error()
		if serr := s.store.SetStatus(ctx, row, claimID, StatusUncertain); serr != nil {
			s.log.Warn("connection: uncertain verdict fenced", zap.Error(serr))
		}
		s.event(ctx, "connection.uncertain", fields)
		if !expired {
			return m.Access, row, nil
		}
		if p.Refresh.OnUncertain == OnUncertainRetry {
			return "", row, fail(CodeBusy, "a renewal of the %s connection had an unknown outcome: try again shortly", row.Provider)
		}
		return "", row, fail(CodeReconnect, "a renewal of the %s connection had an unknown outcome and its token has expired: the person reconnects it", row.Provider)
	}
}

// renewLater renews in the background: the token is still valid, so this
// request need not wait. The claim is already this node's.
func (s *Service) renewLater(ctx context.Context, tenant, declStack string, p *Provider, row Row, claimID string, m Material) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), RequestTimeout+5*time.Second)
	go func() {
		defer cancel()
		if _, _, err := s.renewNow(bg, tenant, declStack, p, row, claimID, m); err != nil {
			s.log.Info("connection: background renewal did not complete", zap.String("provider", row.Provider), zap.String("code", err.Code))
		}
	}()
}

// waitRenewed waits, briefly, for the claimant's new generation.
func (s *Service) waitRenewed(ctx context.Context, p *Provider, row Row) (string, Row, *Error) {
	deadline := s.now().Add(busyWait)
	for {
		latest := s.reload(ctx, row)
		if latest.Generation > row.Generation {
			m, err := s.store.Open(ctx, latest)
			if err != nil {
				return "", latest, fail(CodeUnavailable, "%v", err)
			}
			return m.Access, latest, nil
		}
		if latest.ClaimID == "" {
			// The claimant finished without a new generation: a verdict.
			switch latest.Status {
			case StatusReconnect:
				return "", latest, fail(CodeReconnect, "%s refused the %s connection's refresh token: the person reconnects it", providerTitle(p, row.Provider), row.Provider)
			default:
				return "", latest, fail(CodeBusy, "the %s connection's token has expired and its renewal did not complete: try again shortly", row.Provider)
			}
		}
		if !s.now().Before(deadline) {
			return "", latest, fail(CodeBusy, "the %s connection's token has expired and another node is renewing it: try again shortly", row.Provider)
		}
		select {
		case <-ctx.Done():
			return "", latest, fail(CodeBusy, "the request ended while the %s connection was being renewed", row.Provider)
		case <-time.After(busyPoll):
		}
	}
}
