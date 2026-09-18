package processor

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/txcguard"
)

// The `_txc.*` write/delete policy — which control-plane paths an
// author-controlled producer may touch — lives in chassis/txcguard, a leaf
// package, so the op handlers (chassis/ops, chassis/server) can apply the same
// policy to an author-chosen output target (`WITH into`, `to`, `output_path`)
// without importing the processor. The wrappers below keep this package's call
// sites reading as before; the allowlists and their rationale are there.
//
// authorWritableTxcPaths is the projection's copy of the author-writable
// subtrees (sanitizeAuthorOutput rebuilds `_txc` from exactly these).
var authorWritableTxcPaths = txcguard.AuthorWritablePaths()

// authorMayWriteTxc reports whether an author-controlled producer (a Tier-2
// executor's output, an EMIT overlay, or a `_txc.delete` target) may write the
// given envelope path. `path` must already be normalized (no leading "."/"@" —
// see normalizeEnvelopePath). See txcguard.AuthorMayWrite: the decision is made
// on the keys sjson will resolve, so an escaped or `:`-forced spelling of a
// reserved path (`\_txc.tenant`, `:_txc.tenant`) is refused like the plain one.
func authorMayWriteTxc(path string) bool { return txcguard.AuthorMayWrite(path) }

// authorMayDeleteTxc is the `_txc.delete` target guard: everything an author
// may write, plus the delete-only inbound facts (txcguard.AuthorMayDelete).
func authorMayDeleteTxc(path string) bool { return txcguard.AuthorMayDelete(path) }

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
func systemMayWriteTxc(path string) bool { return txcguard.SystemMayWrite(path) }

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
// compute, MCP tools, workspaces, and rule-author mocks (note: `txco://mock`
// reports transport "mock", NOT "txco") — is untrusted. The workspace
// transport is untrusted like the rest but carries a chassis-authored
// provenance stamp; sanitizeAuthorOutputFor widens its allowlist by exactly
// that subtree rather than trusting the transport.
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

// workspaceWritableTxcPaths is the one subtree the workspace transport may
// carry past the author-output sanitizer: the provenance stamp
// (`_txc.workspace.{provider,computer,run,exit,duration_ms}`) that
// ExecWorkspace itself authors. The sandbox never produces envelope JSON —
// its stdout is a string under `WITH into` — so nothing author-controlled
// can reach this path; the allowance is keyed on the transport the dispatch
// switch took, never on the output's own claims. Deliberately NOT added to
// the trusted set: workspace stays an untrusted transport for every other
// reserved path.
var workspaceWritableTxcPaths = []string{
	"workspace", // chassis-authored provenance for a workspace:// dispatch
}

// sanitizeAuthorOutputFor is sanitizeAuthorOutput with the per-transport
// allowance: transport "workspace" keeps `_txc.workspace.*`; every other
// author-controlled transport gets the plain projection.
func sanitizeAuthorOutputFor(transport, raw string) string {
	if transport == "workspace" {
		return projectTxcAllowed(raw, authorWritableTxcPaths, workspaceWritableTxcPaths)
	}
	return sanitizeAuthorOutput(raw)
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
		return sanitizeAuthorOutputFor(transport, raw)
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
