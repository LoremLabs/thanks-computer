// Package mail implements the bundled txco://sendmail op: it reads the
// `_sendmail` envelope contract a rule assembled, renders an email (a shell
// template wrapping the body — the bundled default, or a caller-supplied
// `_sendmail.templates.html`), enforces the per-tenant campaign at-most-once
// guard, verifies the From domain, submits to a relay, and emits a usage line.
// The common case is to/subject/body/from; attachments and policy caps are
// later phases (see internal docs/todo-sendmail.md).
package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	htmltemplate "html/template"
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
	RateLimits    string // per-tenant send caps, e.g. "100/2m,200/4h"; empty disables
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
	relayOK       bool         // a relay is configured
	rl            *rateLimiter // per-tenant, per-node send caps; nil = disabled
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
		rl:            newRateLimiter(parseRateRules(cfg.RateLimits)),
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
	textTmpl := s.Get("text").String()            // explicit plaintext part; else derived from the HTML body
	htmlShell := s.Get("templates.html").String() // optional custom HTML shell; else the bundled default
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

	// Cc (visible header) + Bcc (envelope-only): flat address lists added to
	// every per-recipient message. The common single-To case is one message
	// plus these extra envelope recipients.
	cc := parseAddrList(s.Get("cc"))
	extraRcpts := make([]string, 0, 4)
	for _, a := range cc {
		extraRcpts = append(extraRcpts, a.Address)
	}
	for _, a := range parseAddrList(s.Get("bcc")) {
		extraRcpts = append(extraRcpts, a.Address)
	}

	// Reply-To (dedicated, ergonomic) + a denylisted map of extra headers.
	// Both are shared across every per-recipient message. The denylist keeps
	// the structural / signing / loop-guard headers sendmail owns off-limits.
	replyTo := sanitizeHeaderValue(s.Get("reply_to").String())
	extraHeaders := parseHeaders(s.Get("headers"))

	// Envelope MAIL FROM (becomes Return-Path). Defaults to the header From, so
	// bounces come back where they're visible. Set `_sendmail.envelope_from =
	// "<>"` (or "") for a null reverse-path — the RFC 3834 posture for
	// auto-replies: a bounce of the reply is itself discarded (no loops), and
	// it's still DMARC-aligned via DKIM (SPF just doesn't apply to a null
	// sender). A real address routes bounces elsewhere (its domain needs SPF or
	// deliverability suffers).
	envFrom := fromAddr.Address
	if ef := s.Get("envelope_from"); ef.Exists() {
		if v := strings.TrimSpace(ef.String()); v == "" || v == "<>" {
			envFrom = ""
		} else if a, perr := mail.ParseAddress(v); perr == nil {
			envFrom = a.Address
		}
	}

	// Optional custom HTML shell (_sendmail.templates.html) — e.g. a Maizzle-built
	// template a stack ships in its FILES and reads at send time. Parsed ONCE here
	// (the body/subject it wraps are per-recipient, executed in the loop); empty
	// falls back to the bundled default. A broken template fails loud rather than
	// silently reverting to the default.
	var customShell *htmltemplate.Template
	if strings.TrimSpace(htmlShell) != "" {
		t, perr := parseShell(htmlShell)
		if perr != nil {
			return errResult("invalid_template", fmt.Sprintf("templates.html: %v", perr)),
				fmt.Errorf("sendmail: invalid templates.html: %w", perr)
		}
		customShell = t
	}

	var results []recipResult
	var sent, skipped, failed int
	for _, r := range recips {
		if !r.parseOK {
			failed++
			results = append(results, recipResult{To: r.raw, Status: "error", Reason: "invalid_address"})
			continue
		}
		// Per-tenant, per-node send cap (runaway-loop safety valve). Checked
		// BEFORE the campaign claim so a throttled recipient leaves no claim to
		// release, and counted only when allowed. Throttled recipients are
		// skipped (no usage); the reason distinguishes a retry-later throttle
		// from a permanent campaign dedup.
		if m.rl != nil && !m.rl.allow(tenant, m.now()) {
			skipped++
			results = append(results, recipResult{To: r.addr.Address, Status: "skipped", Reason: "rate_limited"})
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
				if customShell != nil {
					full, rerr = renderShell(customShell, subj, bh, "", r.vars)
				} else {
					full, rerr = renderDefault(subj, bh, "", r.vars)
				}
			}
			if rerr == nil {
				// Explicit _sendmail.text wins (rendered per-recipient, newlines
				// preserved); otherwise derive a faithful plaintext from the body.
				text := htmlToText(bodyHTML)
				if textTmpl != "" {
					if rendered, terr := renderText(textTmpl, r.vars); terr != nil {
						rerr = terr
					} else if strings.TrimSpace(rendered) != "" {
						text = rendered
					}
				}
				msg, msgID, merr := composeMIME(*fromAddr, r.addr, cc, replyTo, extraHeaders, subj, full, text, fromDomain)
				if merr != nil {
					rerr = merr
				} else if rerr == nil {
					// DKIM-sign (per-domain key) before handing to the relay.
					msg = m.dkimSign(ctx, fromDomain, msg)
					rcpts := append([]string{r.addr.Address}, extraRcpts...)
					if serr := m.submit(ctx, envFrom, rcpts, msg); serr != nil {
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

// parseAddrList parses a `cc`/`bcc` value (a string or an array of address
// strings) into mail.Addresses, skipping anything unparseable. These are flat
// lists (no per-recipient vars): Cc becomes a visible header, Bcc is
// envelope-only.
func parseAddrList(v gjson.Result) []mail.Address {
	var out []mail.Address
	add := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		if a, err := mail.ParseAddress(s); err == nil {
			out = append(out, *a)
		}
	}
	switch {
	case v.IsArray():
		for _, el := range v.Array() {
			add(el.String())
		}
	case v.Type == gjson.String:
		add(v.String())
	}
	return out
}

// protectedHeaders are the headers sendmail owns — structural, signing,
// envelope, or loop-guard. A user-supplied `headers` map (and `reply_to`) can
// never set these: overriding them would break the MIME structure, invalidate
// the DKIM signature, or defeat the auto-reply loop guards. Matched
// case-insensitively.
var protectedHeaders = map[string]bool{
	"from": true, "to": true, "cc": true, "bcc": true, "sender": true,
	"reply-to":     true, // use the dedicated _sendmail.reply_to field
	"subject":      true,
	"date":         true,
	"message-id":   true,
	"mime-version": true, "content-type": true,
	"content-transfer-encoding": true, "content-disposition": true,
	"auto-submitted": true, "x-auto-response-suppress": true,
	"dkim-signature": true, "received": true, "return-path": true,
}

// sanitizeHeaderValue trims and strips CR/LF so a value can't smuggle in extra
// header lines (header injection).
func sanitizeHeaderValue(v string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(strings.TrimSpace(v))
}

// parseHeaders reads the `headers` object ({name: value}) into a sanitized map,
// dropping protected, malformed, or empty entries. The MIME writer canonicalizes
// names; we just reject anything with structural chars in the name.
func parseHeaders(v gjson.Result) map[string]string {
	if !v.IsObject() {
		return nil
	}
	out := map[string]string{}
	v.ForEach(func(k, val gjson.Result) bool {
		name := strings.TrimSpace(k.String())
		if name == "" || strings.ContainsAny(name, "\r\n: ") || protectedHeaders[strings.ToLower(name)] {
			return true
		}
		if value := sanitizeHeaderValue(val.String()); value != "" {
			out[name] = value
		}
		return true
	})
	if len(out) == 0 {
		return nil
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
