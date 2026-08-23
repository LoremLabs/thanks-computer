package processor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The `_txc.*` namespace is the chassis control plane carried inside every
// envelope: identity and routing (`_txc.tenant`, `_txc.src`, `_txc.rid`,
// `_txc.route.*`), budget (`_txc.fuel_used`, `_txc.ttl`, `_txc._seen`), inbound
// request facts (`_txc.web.req.*`, `_txc.lmtp.*`, `_txc.client.*`), computed
// auth results (`_txc.computed.*`), and billing telemetry (`_txc.chat.*`).
//
// Most of it must NOT be settable by a tenant author. But a few `_txc.*` paths
// are legitimately produced by author-controlled code (the rendered response,
// flow control). authorMayWriteTxc draws that line: it is the single source of
// truth for "may an author-controlled producer write this envelope path?", used
// by the Tier-2 output sanitizer, the EMIT overlay, and the `_txc.delete`
// target guard.
//
// The policy is default-CLOSED: only the paths listed here are author-writable
// under `_txc.`; everything else (including any field added later) is reserved.
// A false reject is a degraded feature; a missing reservation is a control-plane
// bypass — so the allowlist is deliberately small and grows only with a test
// that proves a shipped resonator needs the path.
var authorWritableTxcPaths = []string{
	"web.res",      // the rendered HTTP response (status/body/headers)
	"lmtp.res",     // the SMTP verdict
	"goto",         // flow control: jump to another stage
	"halt",         // flow control: stop the pipeline
	"delete",       // prune envelope paths (targets are separately guarded)
	"telemetry",    // tenant metric intents (_txc.telemetry.metrics), consumed post-request
	"llm.reject",   // AI gateway: stack-shaped rejection (status/type/message)
	"llm.upstream", // AI gateway: upstream base-URL override
	"llm.headers",  // AI gateway: extra upstream request headers
	"llm.context",  // AI gateway: stack-emitted context items the gateway serializes into system blocks
}

// authorMayWriteTxc reports whether an author-controlled producer (a Tier-2
// executor's output, an EMIT overlay, or a `_txc.delete` target) may write the
// given envelope path. `path` must already be normalized (no leading "."/"@" —
// see normalizeEnvelopePath). Non-`_txc` paths are always writable; under
// `_txc.` only the allowlisted subtrees are.
//
// `_txc.ttl` is intentionally NOT here: it is writable only via EMIT, and only
// lowered (the IP-TTL idiom), which OverlayResponse handles as a special case.
// A remote/compute/mock producer has no legitimate need to touch the chassis
// budget, so it cannot.
func authorMayWriteTxc(path string) bool {
	if path != "_txc" && !strings.HasPrefix(path, "_txc.") {
		return true // non-_txc keys are the author's own data
	}
	sub := strings.TrimPrefix(path, "_txc.")
	for _, allowed := range authorWritableTxcPaths {
		if sub == allowed || strings.HasPrefix(sub, allowed+".") {
			return true
		}
	}
	return false
}

// authorDeletableTxcPaths are reserved `_txc.*` paths an author may DELETE
// (via `EMIT @delete`) but never write: inbound request facts the chassis
// stamped, which an op has consumed and wants out of the envelope before it
// is copied per step / stored in a continuation payload. `web.req.body` is
// the raw inbound body (base64, up to --web-max-body-bytes) — after
// txco://blob/put or a parser has eaten it, carrying 30 MiB through the
// rest of the flow is pure cost. Deleting a stamped fact can't forge one,
// so this list is broader than the write allowlist but still explicit.
var authorDeletableTxcPaths = []string{
	"web.req.body", // the inbound HTTP body, consumed by blob/put / parsers
}

// authorMayDeleteTxc is the `_txc.delete` target guard: everything an
// author may write, plus the delete-only facts above. Same normalized-path
// contract as authorMayWriteTxc.
func authorMayDeleteTxc(path string) bool {
	if authorMayWriteTxc(path) {
		return true
	}
	sub := strings.TrimPrefix(path, "_txc.")
	for _, allowed := range authorDeletableTxcPaths {
		if sub == allowed || strings.HasPrefix(sub, allowed+".") {
			return true
		}
	}
	return false
}

// systemMayWriteTxc reports whether a SYSTEM-authored rule — one executing in
// a run pinned to the `_sys` tenant, i.e. the boot pipeline — may EMIT the
// given envelope path in ADDITION to the author-writable set. This is the
// boot operator-hook contract: a `_sys/boot` hook proposes a route
// (`EMIT @route.tenant/@route.stack/@route.to`, e.g. the shipped
// examples' path-gated auto-routes), boot/100's txco://route promotes the
// proposal, and maybeRetenant — gated on the same chassis-pinned `_sys`
// tenant — performs the actual one-way re-tenant.
//
// Tenant-authored rules can never reach this branch: the pin is stamped at
// ingress from chassis data (StampEnvelope overwrites any client value), it
// only ever changes one-way _sys→tenant, tenants cannot create `_`-prefixed
// slugs (tenants.ReservedSlug), and OpsForStage filters every lookup by the
// pin — a `_sys`-pinned run executes only `tnt_sys`-owned rows.
//
// Same matching shape as authorMayWriteTxc so lookalikes (`_txc.routes`,
// `_txc.route_x`) stay reserved. Deliberately NOT merged into
// authorWritableTxcPaths: that list is shared with the output sanitizer,
// SET POST, and the delete guards, which all stay closed to `route.*`.
func systemMayWriteTxc(path string) bool {
	sub := strings.TrimPrefix(path, "_txc.")
	return sub == "route" || strings.HasPrefix(sub, "route.")
}

// transportAuthorControlled reports whether output produced by the given
// dispatch transport (the string Exec stamps on every step) is
// author-controlled and therefore must be sanitized of reserved `_txc.*`
// control fields before it merges into the envelope.
//
// Trusted producers — built-in core handlers resolved through the chassis Mux
// registry (`txco`), the chassis-owned `ai://` namespace, and chassis-
// synthesized control outputs (`goto` stage jumps, `noop`) — may write reserved
// control fields (e.g. txco://hmac-verify writes `_txc.computed.sig_valid`,
// ai:// writes `_txc.chat.tokens.*`). Everything else — remote HTTP, sandboxed
// compute, MCP tools, and rule-author mocks (note: `txco://mock` reports
// transport "mock", NOT "txco") — is untrusted.
//
// Trust is keyed off the transport the dispatch switch actually took (see
// Exec), so it can never drift from the routing decision the way a re-derived
// scheme check would.
func transportAuthorControlled(transport string) bool {
	switch transport {
	case "txco", "ai", "goto", "noop":
		return false
	default:
		// mock, http, https, compute, mcp+http, unsupported, "" (goto:// TODO)
		return true
	}
}

// trustedWritableTxcPaths extends the author-writable set for output that a
// TRUSTED transport produced but that reaches the envelope via a stored
// continuation terminal (Resume's merge) rather than the live sync merge.
// The sync path merges trusted output raw; the resume path instead projects
// through this widened allowlist so the unforgeable core — `_txc.tenant`,
// `_txc.fuel_used`, `_txc.ttl`, `_txc._seen`, `_txc.rid`, `_txc.src`,
// `_txc.route.*`, `_txc.runtime.*` — can never be replayed out of the store,
// even by bytes a trusted handler once wrote (defense in depth: the store
// outlives the request that validated it).
//
// Same growth policy as authorWritableTxcPaths: only subtrees a shipped
// trusted handler provably writes, added with a test. Currently: `_txc.chat.*`
// (ai://chat billing/observability stamps) and `_txc.computed.*` (txco://
// hmac-sign/-verify, basic-auth-encode results).
var trustedWritableTxcPaths = []string{
	"chat",     // ai://chat: provider/model/tokens/latency/retries/routing
	"computed", // txco:// auth helpers: sig, sig_valid, basic_auth, …
}

// sanitizeAuthorOutput projects an author-controlled producer's output down to
// what it is allowed to write: every non-`_txc` key verbatim, plus only the
// allowlisted `_txc.*` subtrees. Reserved `_txc.*` fields are dropped; if
// nothing allowed remains under `_txc`, the `_txc` object is omitted entirely.
//
// Projection (rebuild-from-allowed) — rather than deleting reserved leaves —
// is what makes nested partial-allow correct: a forged sibling
// (`_txc.tenant`) is dropped while an allowed sibling (`_txc.web.res`) is
// preserved, and no empty reserved parent is ever left behind. It also closes
// the null-merge vector: `{"_txc":{"tenant":null}}` simply isn't in the
// allowlist, so it can't reach MergeJSON to null the real value.
func sanitizeAuthorOutput(raw string) string {
	return projectTxcAllowed(raw, authorWritableTxcPaths, nil)
}

// sanitizeTrustedOutput is the resume-merge projection for terminals a
// trusted transport produced: author-writable paths plus the trusted set.
func sanitizeTrustedOutput(raw string) string {
	return projectTxcAllowed(raw, authorWritableTxcPaths, trustedWritableTxcPaths)
}

// sanitizeTerminalOutput picks the projection for a stored continuation
// terminal by the transport that produced it (OpTerminal.Transport). An empty
// transport — worker-callback terminals and pre-transport-field docs — is
// author-controlled per transportAuthorControlled's fail-closed default.
func sanitizeTerminalOutput(transport, raw string) string {
	if transportAuthorControlled(transport) {
		return sanitizeAuthorOutput(raw)
	}
	return sanitizeTrustedOutput(raw)
}

func projectTxcAllowed(raw string, allowlists ...[]string) string {
	if raw == "" {
		return raw
	}
	if !gjson.Get(raw, "_txc").Exists() {
		return raw // nothing reserved to strip
	}
	out, err := sjson.Delete(raw, "_txc")
	if err != nil {
		return raw
	}
	for _, list := range allowlists {
		for _, p := range list {
			// allowlist keys contain no dots, so no gjson/sjson escaping is needed.
			if v := gjson.Get(raw, "_txc."+p); v.Exists() {
				if set, serr := sjson.SetRaw(out, "_txc."+p, v.Raw); serr == nil {
					out = set
				}
			}
		}
	}
	return out
}
