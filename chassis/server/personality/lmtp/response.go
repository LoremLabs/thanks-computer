package lmtp

import (
	"github.com/emersion/go-smtp"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
)

// admissionSMTP maps a shared-gate admission denial to an SMTP verdict, or
// nil when the request wasn't gate-denied. A suspended/over-limit tenant
// (and a draining node) map to a TEMPORARY 451 by default, so the sender's
// MTA queues and retries rather than bouncing — important for a non-payment
// suspension that may clear. Only a hard-disabled/forbidden tenant (403)
// maps to a PERMANENT 550. This precedes any rule verdict: a denied tenant
// never reaches its stack, so there is no rule verdict to honor.
func admissionSMTP(raw string) *smtp.SMTPError {
	status, reason, ok := admission.Denied(raw)
	if !ok {
		return nil
	}
	code := 451
	if status == 403 {
		code = 550
	}
	msg := reason
	if msg == "" {
		msg = defaultMsgFor(code)
	}
	return &smtp.SMTPError{
		Code:         code,
		EnhancedCode: enhancedFor(code),
		Message:      msg,
	}
}

// broadcastVerdict reads `_txc.lmtp.res.{code,msg}` from the pipeline
// response and turns it into a smtp.SMTPError (or nil for 2xx).
//
// Default-deny: if the pipeline didn't set an explicit code, every
// recipient gets 550 5.1.1 "no rule accepted this recipient". An
// opstack must affirmatively SET `_txc.lmtp.res.code = 250` to accept
// a delivery. Rationale (from internal docs/todo-lmtp.md):
//   - 250-by-default silently blackholes mail (no bounce, sender
//     believes delivery succeeded, message lost).
//   - 4xx-by-default makes Postfix queue and retry for days before
//     bouncing.
//   - 550 produces a proper DSN immediately.
//
// Per-recipient verdicts (`_txc.lmtp.res.recipients[]`) land in Phase 3
// when this session implements smtp.LMTPSession. Phase 0 broadcasts
// the single `_txc.lmtp.res.{code,msg}` to every recipient — the
// "treat all the same" common case.
func broadcastVerdict(raw string) error {
	if raw == "" || !gjson.Valid(raw) {
		return defaultDeny()
	}

	code := gjson.Get(raw, "_txc.lmtp.res.code").Int()
	msg := gjson.Get(raw, "_txc.lmtp.res.msg").String()

	if code == 0 {
		return defaultDeny()
	}

	// 2xx: accept the delivery. go-smtp emits "250 OK" (or whatever
	// status text the library uses) for a nil return.
	if code >= 200 && code < 300 {
		return nil
	}

	if msg == "" {
		msg = defaultMsgFor(int(code))
	}

	return &smtp.SMTPError{
		Code:         int(code),
		EnhancedCode: enhancedFor(int(code)),
		Message:      msg,
	}
}

func defaultDeny() *smtp.SMTPError {
	return &smtp.SMTPError{
		Code:         550,
		EnhancedCode: smtp.EnhancedCode{5, 1, 1},
		Message:      "no rule accepted this recipient",
	}
}

// computeVerdicts produces one verdict per recipient, in the same
// order as the RCPT TO commands. Resolution hierarchy (default-deny
// throughout — see also `defaultDeny`):
//
//  1. If the pipeline response has `_txc.lmtp.res.recipients[]`,
//     each rcpt[i] gets:
//     - recipients[i].{code,msg} if present and valid, OR
//     - 550 (default-deny) for missing slots — explicit accept is
//       required *per recipient*. A short array does NOT broadcast
//       the last entry forward; that would let an "accept the first
//       one" rule silently accept later recipients it never
//       considered. Length-mismatch logs a warning.
//
//  2. Else if the pipeline response has `_txc.lmtp.res.{code,msg}`,
//     that verdict broadcasts to every recipient (the "treat all
//     the same" common case).
//
//  3. Else: every recipient gets 550 (default-deny).
//
// Returned slice always has len(rcpts) entries. A nil entry means
// "accept" (2xx); a non-nil *smtp.SMTPError carries the failure.
// Per-recipient codes need NOT all be the same — the smtp.LMTPSession
// path writes them out individually via StatusCollector.SetStatus.
func computeVerdicts(rcpts []string, raw string, logger *zap.Logger) []*smtp.SMTPError {
	verdicts := make([]*smtp.SMTPError, len(rcpts))

	if raw == "" || !gjson.Valid(raw) {
		for i := range verdicts {
			verdicts[i] = defaultDeny()
		}
		return verdicts
	}

	// Shared admission gate denial takes precedence over any rule verdict
	// (the tenant's stack never ran). Broadcast the mapped SMTP code —
	// 451 by default so mail retries through a transient suspension.
	if se := admissionSMTP(raw); se != nil {
		for i := range verdicts {
			verdicts[i] = se
		}
		return verdicts
	}

	recipients := gjson.Get(raw, "_txc.lmtp.res.recipients")
	if recipients.Exists() && recipients.IsArray() {
		entries := recipients.Array()
		if len(entries) > len(rcpts) && logger != nil {
			logger.Warn("lmtp res.recipients longer than rcpt list; ignoring extras",
				zap.Int("got", len(entries)),
				zap.Int("want", len(rcpts)))
		}
		if len(entries) < len(rcpts) && logger != nil {
			logger.Warn("lmtp res.recipients shorter than rcpt list; missing slots default to 550",
				zap.Int("got", len(entries)),
				zap.Int("want", len(rcpts)))
		}
		for i := range rcpts {
			if i >= len(entries) {
				verdicts[i] = defaultDeny()
				continue
			}
			entry := entries[i]
			code := entry.Get("code").Int()
			if code == 0 {
				// Slot present but no code — same default-deny
				// posture as a missing slot. The rule author left
				// the verdict blank; we err on the side of "don't
				// silently accept".
				verdicts[i] = defaultDeny()
				continue
			}
			if code >= 200 && code < 300 {
				verdicts[i] = nil // accept
				continue
			}
			msg := entry.Get("msg").String()
			if msg == "" {
				msg = defaultMsgFor(int(code))
			}
			verdicts[i] = &smtp.SMTPError{
				Code:         int(code),
				EnhancedCode: enhancedFor(int(code)),
				Message:      msg,
			}
		}
		return verdicts
	}

	// No `recipients[]` array — fall back to the broadcast/default
	// verdict from Phase 0 and stamp it on every recipient.
	v := broadcastVerdict(raw)
	for i := range verdicts {
		verdicts[i] = errToSMTP(v)
	}
	return verdicts
}

// errToSMTP narrows the error returned by broadcastVerdict (which is
// declared as `error` for the Session.Data signature) back to its
// underlying *smtp.SMTPError. nil stays nil (accept).
func errToSMTP(err error) *smtp.SMTPError {
	if err == nil {
		return nil
	}
	if se, ok := err.(*smtp.SMTPError); ok {
		return se
	}
	// broadcastVerdict only ever returns *smtp.SMTPError or nil; this
	// branch is defense-in-depth for future maintainers.
	return defaultDeny()
}

// defaultMsgFor returns a generic status string for a code the rule
// emitted without an accompanying msg. Better than emitting empty
// status text that some MTAs choke on.
func defaultMsgFor(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "OK"
	case code >= 400 && code < 500:
		return "temporary failure"
	case code >= 500:
		return "rejected"
	default:
		return "rejected"
	}
}

// enhancedFor picks a sane RFC 3463 enhanced status code for a given
// SMTP basic code when the rule didn't supply one. The library will
// emit `<code> <enhanced> <msg>` on the wire.
func enhancedFor(code int) smtp.EnhancedCode {
	switch {
	case code >= 200 && code < 300:
		return smtp.EnhancedCode{2, 0, 0}
	case code == 421:
		return smtp.EnhancedCode{4, 3, 2}
	case code == 451:
		return smtp.EnhancedCode{4, 3, 0}
	case code == 452:
		return smtp.EnhancedCode{4, 2, 2}
	case code >= 400 && code < 500:
		return smtp.EnhancedCode{4, 0, 0}
	case code == 550:
		return smtp.EnhancedCode{5, 1, 1}
	case code == 552:
		return smtp.EnhancedCode{5, 3, 4}
	case code == 553:
		return smtp.EnhancedCode{5, 1, 3}
	case code == 554:
		return smtp.EnhancedCode{5, 0, 0}
	default:
		return smtp.EnhancedCode{5, 0, 0}
	}
}
