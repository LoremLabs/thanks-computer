// Package txcguard is the single source of truth for which `_txc.*` envelope
// paths an author-controlled producer may write or delete.
//
// The `_txc.*` namespace is the chassis control plane carried inside every
// envelope: identity and routing (`_txc.tenant`, `_txc.src`, `_txc.rid`,
// `_txc.route.*`), budget (`_txc.fuel_used`, `_txc.ttl`, `_txc._seen`), inbound
// request facts (`_txc.web.req.*`, `_txc.lmtp.*`, `_txc.client.*`), computed
// auth results (`_txc.computed.*`), and billing telemetry (`_txc.chat.*`).
//
// Most of it must NOT be settable by a tenant author. But a few `_txc.*` paths
// are legitimately produced by author-controlled code (the rendered response,
// flow control). AuthorMayWrite draws that line for every place an author can
// name a destination: the Tier-2 output sanitizer, the EMIT overlay, SET POST,
// LOOP SET, the `_txc.delete` target guard (all in chassis/processor), and the
// author-chosen output target of a trusted `txco://` op (`WITH into`, `to`,
// `output_path` — see AuthorTarget / ComputedTarget).
//
// It is a leaf package so both the processor and the op handlers
// (chassis/ops, chassis/server) can share one policy without an import cycle.
//
// The policy is default-CLOSED: only the paths listed here are author-writable
// under `_txc.`; everything else (including any field added later) is reserved.
// A false reject is a degraded feature; a missing reservation is a control-plane
// bypass — so the allowlist is deliberately small and grows only with a test
// that proves a shipped resonator needs the path.
package txcguard

import (
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// authorWritable lists the `_txc.` subtrees an author may write. Entries are
// plain dotted key paths (no escapes); matching is by whole key, so lookalikes
// (`_txc.web.resx`, `_txc.gotox`) stay reserved.
var authorWritable = []string{
	"web.res",      // the rendered HTTP response (status/body/headers)
	"lmtp.res",     // the SMTP verdict
	"dns.res",      // the DNS answer for a stack-answered zone (rcode/answer/authority)
	"imap.res",     // the IMAP answer-lane verdict (ok/msg/code/flags/object_key)
	"source.res",   // the source inlet verdict: a per-item action override (none/seen/move:<dest>)
	"calendar.res", // the calendar answer-lane verdict (ok/msg/code/event/ical)
	"contacts.res", // the contacts answer-lane verdict (ok/msg/code/card/vcard)
	"tcp.res",      // the tcp head's verdict for one connect/line run (write/action)
	"goto",         // flow control: jump to another stage
	"halt",         // flow control: stop the pipeline
	"delete",       // prune envelope paths (targets are separately guarded)
	"telemetry",    // tenant metric intents (_txc.telemetry.metrics), consumed post-request
	"llm.reject",   // AI gateway: stack-shaped rejection (status/type/message)
	"llm.upstream", // AI gateway: upstream base-URL override
	"llm.headers",  // AI gateway: extra upstream request headers
	"llm.context",  // AI gateway: stack-emitted context items the gateway serializes into system blocks
}

// authorDeletable are reserved `_txc.*` paths an author may DELETE (via
// `EMIT @delete`) but never write: inbound request facts the chassis stamped,
// which an op has consumed and wants out of the envelope before it is copied
// per step / stored in a continuation payload. `web.req.body` is the raw
// inbound body (base64, up to --web-max-body-bytes) — after txco://blob/put or
// a parser has eaten it, carrying 30 MiB through the rest of the flow is pure
// cost. Deleting a stamped fact can't forge one, so this list is broader than
// the write allowlist but still explicit.
var authorDeletable = []string{
	"web.req.body",       // the inbound HTTP body, consumed by blob/put / parsers
	"imap.msg.text",      // an appended message's bodies + headers: consumed by the
	"imap.msg.html",      // _imap stack, then omitted from the trace/continuation
	"imap.msg.headers",   // payload (the record is in the store already)
	"calendar.ical",      // a client's calendar object and its parse: consumed by
	"calendar.event",     // the _calendar stack, then omitted from the trace
	"calendar.prior",     // (the store holds the bytes)
	"contacts.vcard",     // a client's card and its parse: consumed by the
	"contacts.card",      // _contacts stack, then omitted from the trace
	"contacts.prior",     // (the store holds the bytes)
	"websocket.msg.text", // an inbound WebSocket message's payload (text, or
	"websocket.msg.data", // base64 binary): consumed, then out of the trace
}

// systemWritable is the extra subtree a SYSTEM-authored rule (a run pinned to
// the `_sys` tenant — the boot pipeline) may EMIT: the routing proposal. The
// processor owns the "is this run system-authored" decision; see its
// systemMayWriteTxc for the full contract.
var systemWritable = []string{"route"}

// computedWritable is the subtree where a trusted auth helper
// (txco://hmac-sign, hmac-verify, basic-auth-encode, basic-auth-verify)
// reports its verdict, at an author-chosen `output_path`. See ComputedTarget.
var computedWritable = []string{"computed"}

// AuthorWritablePaths returns a copy of the author-writable `_txc.` subtrees,
// for the processor's output projection (which rebuilds `_txc` from exactly
// these).
func AuthorWritablePaths() []string {
	return append([]string(nil), authorWritable...)
}

// NormalizePath turns a txcl-authored envelope path (a `WITH into=…` value, a
// `_txc.delete` entry) into an sjson path: a leading `@` (txcl sugar for
// `._txc.`) expands, and a leading `.` is dropped. "" stays "" (no path).
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "@") {
		p = "_txc." + strings.TrimPrefix(p[1:], ".")
	}
	return strings.TrimPrefix(p, ".")
}

// txcSubKeys resolves path to the object keys sjson will address and reports
// how it relates to the control plane:
//
//	reserved=false           the path is outside `_txc` — the author's own data
//	reserved=true, sub       the keys UNDER `_txc` (empty for `_txc` itself)
//	ok=false                 sjson rejects the path ("" or a bare `*` `?` `#`)
//
// The decision is made on the resolved keys, never on the author's spelling:
// sjson unescapes `\x` to `x` and drops a segment's leading `:`, so
// `\_txc.tenant`, `_tx\c.tenant` and `:_txc.tenant` all address `_txc.tenant`
// and must be judged as such. A prefix test on the raw string let all three
// through.
func txcSubKeys(path string) (sub []string, reserved, ok bool) {
	keys, ok := jsonx.PathKeys(path)
	if !ok {
		return nil, false, false
	}
	if keys[0] != "_txc" {
		return nil, false, true
	}
	return keys[1:], true, true
}

// Each allowlist pre-split into keys, so a check allocates nothing for them.
var (
	authorKeys   = splitAll(authorWritable)
	deleteKeys   = splitAll(authorDeletable)
	systemKeys   = splitAll(systemWritable)
	computedKeys = splitAll(computedWritable)
)

func splitAll(list []string) [][]string {
	out := make([][]string, len(list))
	for i, entry := range list {
		out[i] = strings.Split(entry, ".")
	}
	return out
}

// under reports whether sub equals, or sits beneath, one of the listed
// subtrees — matched key by key.
func under(sub []string, lists ...[][]string) bool {
	for _, list := range lists {
	next:
		for _, want := range list {
			if len(sub) < len(want) {
				continue
			}
			for i, k := range want {
				if sub[i] != k {
					continue next
				}
			}
			return true
		}
	}
	return false
}

// mayTouch is the shared shape of every check below: non-`_txc` paths are the
// author's own data; under `_txc` only the listed subtrees pass; a path sjson
// would reject fails closed (nothing would be written anyway).
func mayTouch(path string, lists ...[][]string) bool {
	// Fast path, allocation-free: nearly every path checked is a plain author
	// key (`.london.time`). With no escape, force-prefix or wildcard byte in
	// it, the first key is simply the text before the first '.', so the
	// prefix test is exact.
	if path != "" && !strings.ContainsAny(path, `\:*?#`) &&
		path != "_txc" && !strings.HasPrefix(path, "_txc.") {
		return true
	}
	sub, reserved, ok := txcSubKeys(path)
	if !ok {
		return false
	}
	if !reserved {
		return true
	}
	return under(sub, lists...)
}

// AuthorMayWrite reports whether an author-controlled producer (a Tier-2
// executor's output, an EMIT overlay, a SET POST, an op's `into`) may write
// the given envelope path. `path` must already be normalized (no leading
// "."/"@" — see NormalizePath). Non-`_txc` paths are always writable; under
// `_txc.` only the allowlisted subtrees are.
//
// `_txc.ttl` is intentionally NOT here: it is writable only via EMIT, and only
// lowered (the IP-TTL idiom), which the processor's OverlayResponse handles as
// a special case. A remote/compute/mock producer has no legitimate need to
// touch the chassis budget, so it cannot.
func AuthorMayWrite(path string) bool {
	return mayTouch(path, authorKeys)
}

// AuthorMayDelete is the `_txc.delete` target guard: everything an author may
// write, plus the delete-only facts in authorDeletable. Same normalized-path
// contract as AuthorMayWrite.
func AuthorMayDelete(path string) bool {
	return mayTouch(path, authorKeys, deleteKeys)
}

// SystemMayWrite reports whether path is in the EXTRA set a system-authored
// rule may EMIT (`_txc.route.*`). It does not include the author-writable set;
// callers check that separately. Unlike the author checks it is false outside
// `_txc`.
func SystemMayWrite(path string) bool {
	sub, reserved, ok := txcSubKeys(path)
	return ok && reserved && under(sub, systemKeys)
}

// AuthorTarget resolves the author-chosen output target of a trusted op — a
// `WITH into = …` or txco://copy's `to`. The op's output merges into the
// envelope unsanitized (trusted transport), so the target itself is the only
// place to stop `into = "@tenant"` from forging a control field.
//
// It returns the normalized sjson path and whether the author may write
// there. raw == "" returns ("", true): no target given, the caller applies
// its default.
func AuthorTarget(raw string) (path string, ok bool) {
	path = NormalizePath(raw)
	if path == "" {
		return "", true
	}
	return path, targetShapeOK(path) && AuthorMayWrite(path)
}

// targetShapeOK holds the two checks a target needs beyond the `_txc` policy:
// no dangling escape, and no numeric key that would pad an array past
// MaxArrayPad. An op builds its result in a fresh `{}`, so that is the
// document the path is judged against.
func targetShapeOK(path string) bool {
	return !danglingEscape(path) && !PadsArray("{}", path)
}

// danglingEscape reports whether path ends in an unpaired `\`. Handlers build
// their result paths by appending to the target (`into + ".items"`), and a
// trailing escape would swallow that separator: `_txc.web.res\` passes as
// `_txc.web.res`, but `_txc.web.res\.items` then addresses the key `res.items`
// under `_txc.web` — outside the subtree that was checked. No legitimate
// target ends this way, so it is refused.
func danglingEscape(path string) bool {
	n := 0
	for i := len(path) - 1; i >= 0 && path[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}

// ComputedTarget is AuthorTarget for the trusted auth helpers' verdict paths
// (`output_path`, `configured_path`), which additionally may sit under
// `_txc.computed.*` — their documented home. The value written there is the
// op's own verdict, never author data, so letting the author pick the leaf
// under `computed` forges nothing. txco://copy and `into` do NOT get this
// allowance: they write author data.
func ComputedTarget(raw string) (path string, ok bool) {
	path = NormalizePath(raw)
	if path == "" {
		return "", true
	}
	return path, targetShapeOK(path) && mayTouch(path, authorKeys, computedKeys)
}
