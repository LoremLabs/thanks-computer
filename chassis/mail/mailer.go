// Package mail implements the bundled txco://sendmail op: it reads the
// `_sendmail` envelope contract a rule assembled, renders an email (the
// bundled default template wrapping the body), enforces the per-tenant
// campaign at-most-once guard, verifies the From domain, submits to a relay,
// and emits a usage line. Phase 1 = the common case (to/subject/body/from);
// custom FILES/ templates, attachments, and policy caps are later phases
// (see internal docs/todo-sendmail.md).
package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// Config carries the relay + limits resolved from chassis config.
type Config struct {
	RelayAddr     string
	RelayTLS      string // "none" | "starttls"
	DialTimeout   time.Duration
	MaxRecipients int
}

// Mailer is the txco://sendmail handler with its injected deps. db is the
// REAL, writable runtime *sql.DB (stable — dbcache only swaps its in-memory
// snapshot, not this handle), used for the campaign claim and the From-domain
// check. usage is nil-safe. submit is injectable (real SMTP in prod, a fake
// in tests).
type Mailer struct {
	db            *sql.DB
	usage         usage.Sink
	log           *zap.Logger
	maxRecipients int
	now           func() time.Time
	submit        SubmitFunc
	relayOK       bool // a relay is configured
}

// NewMailer builds a Mailer. db must be the real runtime DB (e.g.
// pu.RuntimeDB); usage/log may be nil.
func NewMailer(db *sql.DB, u usage.Sink, log *zap.Logger, cfg Config) *Mailer {
	max := cfg.MaxRecipients
	if max <= 0 {
		max = 50
	}
	return &Mailer{
		db:            db,
		usage:         u,
		log:           log,
		maxRecipients: max,
		now:           time.Now,
		submit:        makeSMTPSubmit(cfg),
		relayOK:       cfg.RelayAddr != "",
	}
}

type recipient struct {
	addr    mail.Address
	norm    string // lowercased, trimmed bare address (campaign key)
	vars    map[string]any
	raw     string // original `to` element (for error reporting)
	parseOK bool
}

type recipResult struct {
	To        string `json:"to"`
	Status    string `json:"status"` // sent | skipped | error
	MessageID string `json:"message_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Send is the op body. tenant is the PINNED request tenant (resolved by the
// caller from the trusted context, NOT the mutable envelope) — load-bearing
// for the campaign key and the From-domain anti-spoof check.
func (m *Mailer) Send(ctx context.Context, tenant string, in []byte) (event.Payload, error) {
	if !m.relayOK {
		return errResult("no_relay", ErrNoRelay.Error()), ErrNoRelay
	}

	s := gjson.GetBytes(in, "_sendmail")
	subject := s.Get("subject").String()
	body := s.Get("body").String()
	fromRaw := s.Get("from").String()
	campaign := s.Get("campaign").String()
	if subject == "" || body == "" || fromRaw == "" {
		return errResult("missing_field", "_sendmail requires subject, body, and from"),
			fmt.Errorf("sendmail: missing required field (subject/body/from)")
	}

	fromAddr, err := mail.ParseAddress(fromRaw)
	if err != nil {
		return errResult("invalid_from", fmt.Sprintf("from %q: %v", fromRaw, err)),
			fmt.Errorf("sendmail: invalid from: %w", err)
	}
	fromDomain := domainOf(fromAddr.Address)
	if fromDomain == "" {
		return errResult("invalid_from", "from has no domain"), fmt.Errorf("sendmail: from has no domain")
	}

	// Anti-spoof: the From domain must be a verified hostname for this tenant.
	ok, verr := m.fromDomainVerified(ctx, tenant, fromDomain)
	if verr != nil {
		return errResult("verify_error", verr.Error()), fmt.Errorf("sendmail: from-domain verify: %w", verr)
	}
	if !ok {
		return errResult("from_not_verified",
				fmt.Sprintf("from domain %q is not a verified hostname for this tenant", fromDomain)),
			fmt.Errorf("sendmail: from domain %q not verified for tenant %q", fromDomain, tenant)
	}

	shared := parseVars(s.Get("vars").Raw)
	recips := parseRecipients(s.Get("to"), shared)
	if len(recips) == 0 {
		return errResult("no_recipients", "_sendmail.to is empty or missing"),
			fmt.Errorf("sendmail: no recipients")
	}
	if len(recips) > m.maxRecipients {
		return errResult("too_many_recipients",
				fmt.Sprintf("%d recipients exceeds the cap of %d", len(recips), m.maxRecipients)),
			fmt.Errorf("sendmail: too many recipients (%d > %d)", len(recips), m.maxRecipients)
	}

	rid, _ := ctx.Value(config.CtxKeyRid).(string)
	stack := gjson.GetBytes(in, "_txc.stack").String()
	if stack == "" {
		stack = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	now := m.now().UTC().Format(time.RFC3339)

	var results []recipResult
	var sent, skipped, failed int
	for _, r := range recips {
		if !r.parseOK {
			failed++
			results = append(results, recipResult{To: r.raw, Status: "error", Reason: "invalid_address"})
			continue
		}
		// Campaign at-most-once claim (per recipient).
		if campaign != "" {
			claimed, cerr := m.claimCampaign(ctx, tenant, campaign, r.norm, now)
			if cerr != nil {
				failed++
				results = append(results, recipResult{To: r.addr.Address, Status: "error", Reason: "campaign_error"})
				if m.log != nil {
					m.log.Warn("sendmail: campaign claim failed", zap.Error(cerr))
				}
				continue
			}
			if !claimed {
				skipped++
				results = append(results, recipResult{To: r.addr.Address, Status: "skipped", Reason: "campaign_already_sent"})
				continue
			}
		}

		subj, rerr := renderSubject(subject, r.vars)
		if rerr == nil {
			var bodyHTML, full string
			bh, berr := renderBody(body, r.vars)
			if berr != nil {
				rerr = berr
			} else {
				bodyHTML = string(bh)
				full, rerr = renderDefault(subj, bh, "")
			}
			if rerr == nil {
				text := htmlToText(bodyHTML)
				msg, msgID, merr := composeMIME(*fromAddr, r.addr, subj, full, text, fromDomain)
				if merr != nil {
					rerr = merr
				} else if serr := m.submit(ctx, fromAddr.Address, r.addr.Address, msg); serr != nil {
					rerr = serr
				} else {
					// Delivered.
					if campaign != "" {
						_ = m.markCampaignSent(ctx, tenant, campaign, r.norm, msgID, now)
					}
					m.emitUsage(rid, tenant, stack)
					sent++
					results = append(results, recipResult{To: r.addr.Address, Status: "sent", MessageID: msgID})
					continue
				}
			}
		}
		// Any render/compose/submit failure for a claimed recipient: release
		// the claim so a retry can re-send, and record the failure.
		if campaign != "" {
			m.releaseCampaign(ctx, tenant, campaign, r.norm)
		}
		failed++
		results = append(results, recipResult{To: r.addr.Address, Status: "error", Reason: shortErr(rerr)})
		if m.log != nil {
			m.log.Warn("sendmail: recipient failed", zap.String("to", r.addr.Address), zap.Error(rerr))
		}
	}

	return buildResult(sent, skipped, failed, results), nil
}

func (m *Mailer) emitUsage(rid, tenant, stack string) {
	if m.usage == nil {
		return
	}
	m.usage.WriteEvent(usage.UsageEvent{
		RID:      rid,
		Tenant:   tenant,
		Src:      "sendmail",
		Stack:    stack,
		Status:   "ok",
		Billable: true,
	})
}

// parseRecipients accepts `to` as a string or an array of (string |
// {email, vars}); per-recipient vars override the shared vars (shallow).
func parseRecipients(to gjson.Result, shared map[string]any) []recipient {
	add := func(out []recipient, email, perVarsRaw string) []recipient {
		r := recipient{raw: email, vars: mergeVars(shared, parseVars(perVarsRaw))}
		a, err := mail.ParseAddress(strings.TrimSpace(email))
		if err == nil {
			r.addr = *a
			r.norm = strings.ToLower(strings.TrimSpace(a.Address))
			r.parseOK = true
		}
		return append(out, r)
	}
	var out []recipient
	switch {
	case to.IsArray():
		for _, el := range to.Array() {
			if el.IsObject() {
				out = add(out, el.Get("email").String(), el.Get("vars").Raw)
			} else {
				out = add(out, el.String(), "")
			}
		}
	case to.Type == gjson.String && to.String() != "":
		out = add(out, to.String(), "")
	}
	return out
}

func parseVars(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	m := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func mergeVars(shared, over map[string]any) map[string]any {
	out := make(map[string]any, len(shared)+len(over))
	for k, v := range shared {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func buildResult(sent, skipped, failed int, recips []recipResult) event.Payload {
	r := `{}`
	r, _ = sjson.Set(r, "_sendmail.result.sent", sent)
	r, _ = sjson.Set(r, "_sendmail.result.skipped", skipped)
	r, _ = sjson.Set(r, "_sendmail.result.failed", failed)
	if recips == nil {
		recips = []recipResult{}
	}
	if b, err := json.Marshal(recips); err == nil {
		r, _ = sjson.SetRaw(r, "_sendmail.result.recipients", string(b))
	}
	return event.Payload{Raw: r, Type: event.JSON}
}

func errResult(reason, msg string) event.Payload {
	r := `{}`
	r, _ = sjson.Set(r, "_sendmail.result.status", "error")
	r, _ = sjson.Set(r, "_sendmail.result.reason", reason)
	r, _ = sjson.Set(r, "_sendmail.result.error", msg)
	return event.Payload{Raw: r, Type: event.JSON}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
