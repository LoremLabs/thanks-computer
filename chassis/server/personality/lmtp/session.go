package lmtp

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"sort"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// lmtpBackend is the chassis-flavored go-smtp Backend. It produces a
// fresh Session per connection, threading the controller through so
// the session can reach pu.Bus / Conf / Logger.
type lmtpBackend struct {
	ctrl *LMTPController
}

func (b *lmtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &lmtpSession{
		ctrl: b.ctrl,
		conn: c,
		// Listener name. v1 supports a single named listener
		// ("default"); multi-listener routing is a follow-up.
		listener: "default",
	}, nil
}

// lmtpSession holds the per-transaction state. The go-smtp library
// calls Mail → Rcpt (N times) → Data (or LMTPData in LMTP mode) →
// Reset (or Logout). State is reset between transactions on the same
// connection.
//
// Implements smtp.Session (required) AND smtp.LMTPSession (optional
// add-on). With Server.LMTP=true, the library prefers LMTPData and
// ignores Data — but Data still has to exist to satisfy Session.
type lmtpSession struct {
	ctrl     *LMTPController
	conn     *smtp.Conn
	listener string

	mailFrom string
	rcpts    []string
}

func (s *lmtpSession) Reset() {
	s.mailFrom = ""
	s.rcpts = nil
}

func (s *lmtpSession) Logout() error {
	return nil
}

func (s *lmtpSession) Mail(from string, _ *smtp.MailOptions) error {
	s.mailFrom = from
	return nil
}

func (s *lmtpSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data is the smtp.Session fallback used by non-LMTP clients (or any
// LMTP server that doesn't also implement smtp.LMTPSession). With
// Server.LMTP=true (always, for us), go-smtp picks LMTPData instead;
// Data is unreachable in practice. We keep it minimal — it just
// broadcasts a default-deny since we can't do per-rcpt routing
// through a single Session.Data error return.
func (s *lmtpSession) Data(r io.Reader) error {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "chassis read error",
		}
	}
	return &smtp.SMTPError{
		Code:         550,
		EnhancedCode: smtp.EnhancedCode{5, 1, 1},
		Message:      "LMTP mode required",
	}
}

// LMTPData is the smtp.LMTPSession entry point and the only real path
// in LMTP mode. It implements per-RCPT independent routing with
// (tenant, stack) batching:
//
//  1. Read + bound + parse the message body once.
//  2. For each rcpt, resolve via MailResolver.ResolveRecipient → either
//     a RouteTarget (route exists) or "unrouted" (550 immediately).
//  3. Group rcpts by (tenant, stack). Each group becomes one envelope
//     with `_txc.lmtp.rcpt` = group sublist, `_txc.lmtp.transaction_rcpt`
//     = the full rcpt list, and `_txc.route.*` pre-stamped so the boot
//     pipeline's detectTenantBody honors the resolution without re-
//     running it.
//  4. Dispatch envelopes sequentially. Each envelope's
//     `_txc.lmtp.res.recipients[]` is indexed within its OWN group's
//     rcpts; stitching maps each group-local verdict back to the
//     original RCPT TO order.
//  5. SetStatus per recipient on the StatusCollector.
//
// Returns nil (every rcpt gets an explicit SetStatus so
// fillRemaining has nothing to fill).
func (s *lmtpSession) LMTPData(r io.Reader, status smtp.StatusCollector) error {
	body, tErr := s.readBody(r)
	if tErr != nil {
		for _, rcpt := range s.rcpts {
			status.SetStatus(rcpt, tErr)
		}
		return nil
	}

	// Resolve each rcpt independently, recording original position.
	type slot struct {
		rcpt        string
		originalIdx int
		target      ingress.RouteTarget
		routed      bool
	}
	slots := make([]slot, len(s.rcpts))
	for i, rcpt := range s.rcpts {
		sl := slot{rcpt: rcpt, originalIdx: i}
		if s.ctrl.resolver != nil {
			if t, ok := s.ctrl.resolver.ResolveRecipient(rcpt, s.listener); ok {
				sl.target = t
				sl.routed = true
			}
		}
		slots[i] = sl
	}

	// Group routed slots by (tenant, stack). The `Ingress` and
	// `Verified` fields are observability metadata — they're carried
	// on the dispatched envelope but DO NOT participate in the
	// grouping key. Otherwise rcpts that resolved to the same
	// (tenant, stack) via different YAML keys (one via exact match,
	// one via @domain wildcard) would split into separate envelopes
	// instead of batching.
	type groupKey struct{ tenant, stack string }
	groups := map[groupKey][]int{}   // groupKey → []index_into_slots
	groupMeta := map[groupKey]slot{} // groupKey → first slot (for Ingress + Verified exemplar)
	var groupOrder []groupKey        // preserves first-seen order for sequential dispatch
	for i, sl := range slots {
		if !sl.routed {
			continue
		}
		k := groupKey{tenant: sl.target.Tenant, stack: sl.target.Stack}
		if _, seen := groups[k]; !seen {
			groupOrder = append(groupOrder, k)
			groupMeta[k] = sl
		}
		groups[k] = append(groups[k], i)
	}

	// Per-rcpt verdicts indexed by original RCPT TO position.
	verdicts := make([]*smtp.SMTPError, len(s.rcpts))
	for i, sl := range slots {
		if !sl.routed {
			verdicts[i] = defaultDeny()
		}
	}

	// Dispatch one envelope per group; stitch verdicts back by
	// original index.
	for _, k := range groupOrder {
		slotIdxs := groups[k]
		groupRcpts := make([]string, len(slotIdxs))
		for j, idx := range slotIdxs {
			groupRcpts[j] = slots[idx].rcpt
		}
		// Carry the first rcpt's Ingress + Verified onto the
		// envelope as the group's exemplar (matters for the
		// `_txc.ingress` observability stamp).
		exemplar := groupMeta[k]
		dk := dispatchKey{
			tenant:     k.tenant,
			stack:      k.stack,
			ingressKey: exemplar.target.Ingress,
			verified:   exemplar.target.Verified,
		}

		groupVerdicts, dispatchErr := s.dispatchGroup(body, dk, groupRcpts)
		if dispatchErr != nil {
			// Transport-level failure for THIS group — same code
			// across the group's recipients only (other groups got
			// dispatched OK).
			for _, idx := range slotIdxs {
				verdicts[idx] = dispatchErr
			}
			continue
		}
		for j, idx := range slotIdxs {
			verdicts[idx] = groupVerdicts[j]
		}
	}

	// One Info line per LMTP delivery so inbound mail is visible in the
	// chassis log regardless of disposition. This matters most under
	// deny-by-default: unrouted recipients never dispatch to the bus, so
	// they produce NO trace — without this line a denied delivery is
	// completely silent on the chassis side. Recipient addresses are NOT
	// logged here (they ride the Postfix log + the trace for routed
	// rcpts); we log connection metadata + disposition counts only.
	accepted, rejected := 0, 0
	for _, v := range verdicts {
		if v == nil {
			accepted++
		} else {
			rejected++
		}
	}
	clientIP := ""
	if addr := s.conn.Conn().RemoteAddr(); addr != nil {
		if ta, ok := addr.(*net.TCPAddr); ok && ta != nil {
			clientIP = ta.IP.String()
		}
	}
	s.ctrl.pu.Logger.Info("lmtp delivery",
		zap.String("client_ip", clientIP),
		zap.String("helo", s.conn.Hostname()),
		zap.Int("rcpts", len(s.rcpts)),
		zap.Int("accepted", accepted),
		zap.Int("rejected", rejected),
		zap.Int("bytes", len(body)))

	// Write verdicts. Order doesn't matter for SetStatus (the library
	// maps rcpt → status by rcpt string), but iterating s.rcpts keeps
	// the call order deterministic for tests and logs.
	for i, rcpt := range s.rcpts {
		var perRcpt error
		if verdicts[i] != nil {
			perRcpt = verdicts[i]
		}
		status.SetStatus(rcpt, perRcpt)
	}
	return nil
}

// readBody pulls the message body off the reader with the configured
// size cap. Returns a transport-level SMTPError for oversized or
// unreadable bodies (the entire transaction fails the same way for
// every rcpt).
func (s *lmtpSession) readBody(r io.Reader) ([]byte, *smtp.SMTPError) {
	limit := int64(s.ctrl.pu.Conf.LMTPMaxMsgBytes)
	if limit <= 0 {
		limit = 26214400
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		s.ctrl.pu.Logger.Warn("lmtp body read error", zap.String("err", err.Error()))
		return nil, &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "chassis read error",
		}
	}
	if int64(len(body)) > limit {
		return nil, &smtp.SMTPError{
			Code:         552,
			EnhancedCode: smtp.EnhancedCode{5, 3, 4},
			Message:      "message too large",
		}
	}
	return body, nil
}

// dispatchKey carries the (tenant, stack) group identity plus the
// exemplar Ingress/Verified observability fields onto the envelope.
type dispatchKey struct {
	tenant     string
	stack      string
	ingressKey string
	verified   bool
}

// dispatchGroup builds one envelope for a (tenant, stack) group,
// pushes it onto the bus, and waits for the response. Returns
// per-rcpt verdicts indexed within `groupRcpts`. A non-nil second
// return is a transport-level error to be broadcast across the group
// (timeout, shutdown). Per-rule verdicts (250/550/etc.) come back via
// computeVerdicts.
func (s *lmtpSession) dispatchGroup(
	body []byte,
	key dispatchKey,
	groupRcpts []string,
) ([]*smtp.SMTPError, *smtp.SMTPError) {
	respTimeout, err := time.ParseDuration(s.ctrl.pu.Conf.LMTPRespTimeout)
	if err != nil || respTimeout <= 0 {
		respTimeout = 30 * time.Second
	}

	rid := hxid.NewTimeSort().String()
	now := time.Now()

	payload, _ := sjson.Set("", "_txc.src", "lmtp")
	payload, _ = sjson.Set(payload, "_txc.rid", rid)
	payload, _ = sjson.Set(payload, "_ts", now.Format(time.RFC3339))
	payload, _ = sjson.Set(payload, "_txc.lmtp.listener", s.listener)
	payload, _ = sjson.Set(payload, "_txc.lmtp.mail.from", s.mailFrom)
	payload, _ = sjson.Set(payload, "_txc.lmtp.mail.size", len(body))
	// Group sublist for routing/verdict purposes.
	payload, _ = sjson.Set(payload, "_txc.lmtp.rcpt", groupRcpts)
	// Full transaction rcpt list — informational. Rules that care
	// about who else was on the delivery (sibling-tenant rcpts,
	// Bcc-style fan-out detection) read this.
	payload, _ = sjson.Set(payload, "_txc.lmtp.transaction_rcpt", s.rcpts)

	// Pre-stamp the route proposal so detectTenantBody short-circuits
	// without re-resolving. The boot/100 route op promotes
	// `_txc.route.*` to `_txc.tenant` / `_txc.stack` / `_txc.goto` —
	// same machinery the HTTP path uses, just with a pre-decided
	// proposal instead of a runtime lookup.
	payload, _ = sjson.Set(payload, "_txc.route.tenant", key.tenant)
	payload, _ = sjson.Set(payload, "_txc.route.stack", key.stack)
	payload, _ = sjson.Set(payload, "_txc.route.ingress", key.ingressKey)
	payload, _ = sjson.Set(payload, "_txc.route.hostname_verified", key.verified)
	payload, _ = sjson.Set(payload, "_txc.route.to", key.stack+"/0")

	// Best-effort MIME parse — same as Phase 1.
	msgJSON, perr := parseMessage(body)
	if perr != nil {
		s.ctrl.pu.Logger.Warn("lmtp mime parse failed",
			zap.String("rid", rid),
			zap.String("err", perr.Error()))
		msgJSON = "{}"
	}
	msgJSON, _ = sjson.Set(msgJSON, "raw",
		base64.StdEncoding.EncodeToString(body))
	payload, _ = sjson.SetRaw(payload, "_txc.lmtp.msg", msgJSON)

	// Bounce / DSN detector for stacks to halt on before auto-replying
	// (`WHEN @lmtp.is_bounce`). Null reverse-path or a delivery-status report.
	payload, _ = sjson.Set(payload, "_txc.lmtp.is_bounce", bounceDetected(s.mailFrom, msgJSON))

	// Inbound spam/auth facts from the upstream Rspamd milter's headers,
	// normalized under `_txc.mail.*` (read as `@mail.*`). Phase 1 is
	// annotate-only: the chassis supplies facts; tenant _mail stacks decide
	// policy in txcl. When Rspamd added no headers (down/skipped), available is
	// false and verdict is "unknown" — mail still flows (milter accepts).
	meta := parseMailHeaders(msgJSON, s.ctrl.spamBands)
	payload, _ = sjson.Set(payload, "_txc.mail.spam.source", "rspamd")
	payload, _ = sjson.Set(payload, "_txc.mail.spam.available", meta.available)
	payload, _ = sjson.Set(payload, "_txc.mail.spam.verdict", meta.verdict)
	if meta.hasScore {
		payload, _ = sjson.Set(payload, "_txc.mail.spam.score", meta.score)
	}
	if len(meta.symbols) > 0 {
		payload, _ = sjson.Set(payload, "_txc.mail.rspamd.symbols", meta.symbols)
	}
	if meta.spf != "" {
		payload, _ = sjson.Set(payload, "_txc.mail.auth.spf", meta.spf)
	}
	if meta.dkim != "" {
		payload, _ = sjson.Set(payload, "_txc.mail.auth.dkim", meta.dkim)
	}
	if meta.dmarc != "" {
		payload, _ = sjson.Set(payload, "_txc.mail.auth.dmarc", meta.dmarc)
	}

	// Connection metadata.
	payload, _ = sjson.Set(payload, "_txc.lmtp.client.helo", s.conn.Hostname())
	if addr := s.conn.Conn().RemoteAddr(); addr != nil {
		if ta, ok := addr.(*net.TCPAddr); ok && ta != nil {
			payload, _ = sjson.Set(payload, "_txc.lmtp.client.ip", ta.IP.String())
			payload, _ = sjson.Set(payload, "_txc.client.ip", ta.IP.String())
		}
	}
	if s.ctrl.pu.Conf.DebugPrivate {
		payload, _ = sjson.Set(payload, "_txc.flag_private", true)
	}

	if s.ctrl.pu.Logger.Core().Enabled(zap.DebugLevel) {
		s.ctrl.pu.Logger.Debug("lmtp dispatch group",
			zap.String("rid", rid),
			zap.String("tenant", key.tenant),
			zap.String("stack", key.stack),
			zap.Strings("group_rcpt", groupRcpts),
			zap.Int("size", len(body)))
	}

	ctx, cancel := context.WithTimeout(s.ctrl.ctx, respTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload)
	envelope := event.PackageJSON(ctx, payload, resCh, "lmtp")
	s.ctrl.pu.Bus <- envelope

	select {
	case res := <-resCh:
		if s.ctrl.pu.Logger.Core().Enabled(zap.DebugLevel) {
			s.ctrl.pu.Logger.Debug("lmtp group res",
				zap.String("rid", rid),
				zap.String("raw", res.Raw))
		}
		return computeVerdicts(groupRcpts, res.Raw, s.ctrl.pu.Logger), nil
	case <-ctx.Done():
		s.ctrl.pu.Logger.Info("lmtp response timeout", zap.String("rid", rid))
		return nil, &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "chassis timeout",
		}
	case <-s.ctrl.ctx.Done():
		return nil, &smtp.SMTPError{
			Code:         421,
			EnhancedCode: smtp.EnhancedCode{4, 3, 2},
			Message:      "chassis shutting down",
		}
	}
}

// sortedGroupKeys is exposed for tests that want a deterministic
// dispatch order. Not used in the hot path.
func sortedGroupKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
