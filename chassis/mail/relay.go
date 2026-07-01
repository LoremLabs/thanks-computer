package mail

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// Relay is the txco://relay op body — the `.forward` primitive. Unlike Send it
// does NOT re-compose or re-DKIM-sign: it ships the ORIGINAL RFC 5322 bytes
// (`_relay.raw`, base64, straight from `@lmtp.msg.raw`) to a new recipient
// untouched, so the sender's surviving DKIM signature keeps DMARC aligned to
// their domain and attachments are preserved. It just rewrites the SMTP
// envelope: MAIL FROM = `_relay.envelope_from` (a return-path the tenant owns,
// so SPF aligns and bounces route somewhere catchable), RCPT = `_relay.to`.
//
// source is the request's PINNED originating inlet (processor.SourceScope) —
// the trusted answer to "which inlet started this", captured at first Run, NOT
// the mutable `_txc.src` envelope field. Because relay ships arbitrary bytes
// out under a verified return-path (far more dangerous than sendmail, which
// forces a verified From + signs), it MUST run only from the inbound-mail path:
// a coerced web/other pipeline could otherwise relay attacker-supplied bytes as
// a spam cannon riding the domain's reputation. So the first thing Relay does
// is refuse unless source == "lmtp".
//
// tenant is the PINNED request tenant (processor.TenantScope), used for the
// defense-in-depth envelope_from-domain-verified check and the campaign guard.
func (m *Mailer) Relay(ctx context.Context, tenant, source string, in []byte) (event.Payload, error) {
	// Provenance gate — the primary security boundary. Only inbound LMTP mail
	// may relay. Everything else (http/cron/…) is refused before any work.
	if source != "lmtp" {
		return relayErr("forbidden_source",
				fmt.Sprintf("relay is only permitted from inbound mail (source=%q)", source)),
			fmt.Errorf("relay: forbidden source %q (only lmtp)", source)
	}
	if !m.relayOK {
		return relayErr("no_relay", ErrNoRelay.Error()), ErrNoRelay
	}

	r := gjson.GetBytes(in, "_relay")
	rawB64 := strings.TrimSpace(r.Get("raw").String())
	toRaw := strings.TrimSpace(r.Get("to").String())
	envRaw := strings.TrimSpace(r.Get("envelope_from").String())
	campaign := r.Get("campaign").String()
	if rawB64 == "" || toRaw == "" || envRaw == "" {
		return relayErr("missing_field", "_relay requires raw, to, and envelope_from"),
			fmt.Errorf("relay: missing required field (raw/to/envelope_from)")
	}

	msg, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		return relayErr("invalid_raw", fmt.Sprintf("raw is not valid base64: %v", err)),
			fmt.Errorf("relay: invalid base64 raw: %w", err)
	}

	toAddr, err := mail.ParseAddress(toRaw)
	if err != nil {
		return relayErr("invalid_to", fmt.Sprintf("to %q: %v", toRaw, err)),
			fmt.Errorf("relay: invalid to: %w", err)
	}
	envAddr, err := mail.ParseAddress(envRaw)
	if err != nil {
		return relayErr("invalid_envelope_from", fmt.Sprintf("envelope_from %q: %v", envRaw, err)),
			fmt.Errorf("relay: invalid envelope_from: %w", err)
	}
	envDomain := domainOf(envAddr.Address)
	if envDomain == "" {
		return relayErr("invalid_envelope_from", "envelope_from has no domain"),
			fmt.Errorf("relay: envelope_from has no domain")
	}

	// Defense-in-depth behind the source gate: the return-path must be a domain
	// the tenant owns (same anti-spoof DB check sendmail runs on its From). The
	// message's own From is the arbitrary original sender — we don't police it,
	// only the envelope we stamp.
	ok, verr := m.fromDomainVerified(ctx, tenant, envDomain)
	if verr != nil {
		return relayErr("verify_error", verr.Error()), fmt.Errorf("relay: envelope_from verify: %w", verr)
	}
	if !ok {
		return relayErr("envelope_from_not_verified",
				fmt.Sprintf("envelope_from domain %q is not a verified hostname for this tenant", envDomain)),
			fmt.Errorf("relay: envelope_from domain %q not verified for tenant %q", envDomain, tenant)
	}

	// Per-tenant, per-node runaway guard.
	if m.rl != nil && !m.rl.allow(tenant, m.now()) {
		return relayErr("rate_limited", "per-tenant send rate exceeded"),
			fmt.Errorf("relay: rate limited for tenant %q", tenant)
	}

	// Campaign at-most-once claim (dedups LMTP redelivery so a retry doesn't
	// double-forward). Keyed on the recipient like sendmail.
	norm := strings.ToLower(strings.TrimSpace(toAddr.Address))
	now := m.now().UTC().Format(time.RFC3339)
	if campaign != "" {
		claimed, cerr := m.claimCampaign(ctx, tenant, campaign, norm, now)
		if cerr != nil {
			return relayErr("campaign_error", cerr.Error()), fmt.Errorf("relay: campaign claim: %w", cerr)
		}
		if !claimed {
			return relayResult("skipped", toAddr.Address, envAddr.Address, "campaign_already_sent"), nil
		}
	}

	// Ship the original bytes verbatim — no compose, no dkimSign.
	if serr := m.submit(ctx, envAddr.Address, []string{toAddr.Address}, msg); serr != nil {
		if campaign != "" {
			m.releaseCampaign(ctx, tenant, campaign, norm) // let a retry re-forward
		}
		return relayErr("submit_error", shortErr(serr)), fmt.Errorf("relay: submit: %w", serr)
	}

	if campaign != "" {
		_ = m.markCampaignSent(ctx, tenant, campaign, norm, "", now)
	}
	rid, _ := ctx.Value(config.CtxKeyRid).(string)
	stack := gjson.GetBytes(in, "_txc.stack").String()
	if stack == "" {
		stack = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	m.emitUsageSrc(rid, tenant, stack, "relay")
	return relayResult("sent", toAddr.Address, envAddr.Address, ""), nil
}

// relayResult builds the `_relay.result` success/skip payload.
func relayResult(status, to, envelopeFrom, reason string) event.Payload {
	r := `{}`
	r, _ = sjson.Set(r, "_relay.result.status", status)
	r, _ = sjson.Set(r, "_relay.result.to", to)
	r, _ = sjson.Set(r, "_relay.result.envelope_from", envelopeFrom)
	if reason != "" {
		r, _ = sjson.Set(r, "_relay.result.reason", reason)
	}
	return event.Payload{Raw: r, Type: event.JSON}
}

// relayErr builds the `_relay.result` error payload (mirrors sendmail's
// errResult shape, namespaced under `_relay`).
func relayErr(reason, msg string) event.Payload {
	r := `{}`
	r, _ = sjson.Set(r, "_relay.result.status", "error")
	r, _ = sjson.Set(r, "_relay.result.reason", reason)
	r, _ = sjson.Set(r, "_relay.result.error", msg)
	return event.Payload{Raw: r, Type: event.JSON}
}
