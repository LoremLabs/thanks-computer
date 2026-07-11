package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/kvtools/valkeyrie/store"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/artifact"
	_ "github.com/loremlabs/thanks-computer/chassis/artifact/filestore" // registers the "file" backend
	"github.com/loremlabs/thanks-computer/chassis/bgservice"
	_ "github.com/loremlabs/thanks-computer/chassis/chat/openai"     // registers the "openai" ai://chat backend
	_ "github.com/loremlabs/thanks-computer/chassis/chat/openrouter" // registers the "openrouter" ai://chat backend
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/storeresolver"
	_ "github.com/loremlabs/thanks-computer/chassis/compute/wazero" // registers the "wazero" engine
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	_ "github.com/loremlabs/thanks-computer/chassis/continuation/filestore" // registers the "file" backend
	"github.com/loremlabs/thanks-computer/chassis/controlapply"
	"github.com/loremlabs/thanks-computer/chassis/controlpublish"
	cronq "github.com/loremlabs/thanks-computer/chassis/cron"
	_ "github.com/loremlabs/thanks-computer/chassis/cron/local" // registers the "local" cron queue backend
	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"    // registers the "open" policy
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private" // registers the "private" policy
	_ "github.com/loremlabs/thanks-computer/chassis/embed/ollama"   // registers the "ollama" ai://embed backend
	_ "github.com/loremlabs/thanks-computer/chassis/embed/openai"   // registers the "openai" ai://embed backend
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/feed"
	_ "github.com/loremlabs/thanks-computer/chassis/feed/filesource" // registers the "file" backend
	_ "github.com/loremlabs/thanks-computer/chassis/feed/nop"        // registers the "nop" backend
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	_ "github.com/loremlabs/thanks-computer/chassis/filecas/filestore" // registers the "file" backend
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/logging"
	"github.com/loremlabs/thanks-computer/chassis/mail"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/ops"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/registry"
	"github.com/loremlabs/thanks-computer/chassis/scheduled"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/server/admin"
	continuationui "github.com/loremlabs/thanks-computer/chassis/server/continuation/ui"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	cronp "github.com/loremlabs/thanks-computer/chassis/server/personality/cron"
	dnsp "github.com/loremlabs/thanks-computer/chassis/server/personality/dns"
	"github.com/loremlabs/thanks-computer/chassis/server/personality/lmtp"
	mailmapp "github.com/loremlabs/thanks-computer/chassis/server/personality/mailmap"
	scheduledp "github.com/loremlabs/thanks-computer/chassis/server/personality/scheduled"
	"github.com/loremlabs/thanks-computer/chassis/server/personality/sweep"
	"github.com/loremlabs/thanks-computer/chassis/server/personality/tcp"
	"github.com/loremlabs/thanks-computer/chassis/server/personality/web"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/kvseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/vecseed"
	"github.com/loremlabs/thanks-computer/chassis/telemetry"
	_ "github.com/loremlabs/thanks-computer/chassis/telemetry/log"  // registers the "log" telemetry exporter
	_ "github.com/loremlabs/thanks-computer/chassis/telemetry/otlp" // registers the "otlp" telemetry exporter
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	txtls "github.com/loremlabs/thanks-computer/chassis/tls"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/usage"
	"github.com/loremlabs/thanks-computer/chassis/vector"
	_ "github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec" // registers the bundled "sqlite" vector backend
)

// defaultEntryStage is where every request enters the chassis: the
// concrete _sys-owned `boot` stack at scope 0. It MUST be concrete
// (not a `boot/%` wildcard) so the boot pipeline runs as an ordered
// sparse-scope sequence (0=detect-tenant, optional 10=operator hooks,
// 20=unrouted 404) — a wildcard pattern resolves a single scope and
// can't chain.
const defaultEntryStage = "boot/0"

// stageNoRoute is a synthetic stage value returned by dispatchEnvelope
// in `--ingress-miss-action=reject` mode. It is the hard pre-processor
// 404 toggle: the bus loop branches on this constant and emits a clean
// 404 without running the boot pipeline at all (an operator who wants
// zero rule-engine cost for unrouted traffic). See emitNoRouteResponse.
const stageNoRoute = "__no_route__"

// dispatchEnvelope decides the per-event entry. Tenant resolution is
// NO LONGER done here in Go — it moved into the txco://detect-tenant
// op the editable _sys/boot stack invokes, so the routing decision is
// a visible, traced pipeline step. Every request is stamped with the
// reserved system tenant and enters the concrete _sys/boot pipeline
// pinned to _sys (so its ops resolve tenant-scoped exactly like a
// routed request); _sys/boot then detects the real tenant and
// re-tenants into it, or 404s an unrouted request itself.
//
// `--ingress-miss-action=reject` is retained as a hard opt-out: skip
// the processor entirely and emit a bare 404 (no boot pipeline). The
// default `fallthrough` is the all-traffic boot model.
//
// Verified=false: the trust signal is set authoritatively by
// detect-tenant once the hostname is resolved; boot/tenant rules read
// _txc.hostname_verified after that.
// detectTenantBody is the txco://detect-tenant transform — the DECIDE
// half of the boot pipeline (scope 0). It resolves the envelope to a
// route and writes it as an *inert proposal* under `_txc.route.*`. It
// deliberately does NOT set `_txc.goto`/`_txc.tenant`, so nothing jumps
// or re-tenants yet: scopes 1–99 can read/modify `_txc.route` (rate-
// limit, deny, override) for every request before `txco://route`
// (scope 100) executes it. On a miss it returns `{}` (no proposal) so
// the gated route op is skipped and _sys/boot serves the 404.
// Extracted so it is unit-testable with a stub resolver.
func detectTenantBody(resolver ingress.Resolver, in []byte) string {
	// Continuation poll. A request carrying `?_txc.continuation=<rcid>`
	// (mapped to _txc.continuation by the web inlet) is a poll for an
	// existing run, not hostname-routed. Propose a jump to the internal
	// _sys `txc-continuation` stack with NO tenant — it stays pinned to
	// _sys; the result is read by the opaque rcid, never by hostname
	// tenant. The route op promotes this to a goto at scope 100.
	// Inlet pre-routed the envelope (today: the LMTP head, which
	// resolves each RCPT independently before constructing the
	// per-(tenant,stack) envelope). Echo the existing proposal back
	// so the boot/100 route op picks it up unchanged; the chassis
	// owns the field, so a hostile inlet can't fake this.
	if gjson.GetBytes(in, "_txc.route.to").String() != "" {
		return `{}`
	}
	if gjson.GetBytes(in, "_txc.continuation").String() != "" {
		raw := "{}"
		raw, _ = sjson.Set(raw, "_txc.route.to", "txc-continuation/0")
		return raw
	}
	// Per-tenant cron tick. The cron controller fans out one envelope
	// per tenant that authored a `_cron` stack, stamping the target
	// slug in `_txc.cron.tenant` (trusted: set by the chassis from the
	// dbcache snapshot, never client input). Propose a route into that
	// tenant's `_cron/0`; boot/100's visible `WHEN @route.to != ""`
	// promotes it and maybeRetenant performs the sanctioned _sys→tenant
	// pin — same machinery as a hostname-routed request, no bypass.
	if gjson.GetBytes(in, "_txc.src").String() == "cron" {
		if ct := gjson.GetBytes(in, "_txc.cron.tenant").String(); ct != "" {
			raw := "{}"
			raw, _ = sjson.Set(raw, "_txc.route.tenant", ct)
			raw, _ = sjson.Set(raw, "_txc.route.stack", "_cron")
			raw, _ = sjson.Set(raw, "_txc.route.ingress", "cron")
			raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", true)
			raw, _ = sjson.Set(raw, "_txc.route.to", "_cron/0")
			return raw
		}
	}
	// Room message. The room inlet (admin POST /v1/tenants/{t}/rooms/{room}/
	// messages) stamps the target slug in `_txc.room.tenant` (trusted: the
	// authenticated URL tenant, never client input). Propose a route into that
	// tenant's `_room/0` — the same sanctioned _sys→tenant pin as cron and
	// hostname routing, no bypass. A tenant with no `_room` stack falls through
	// to the 404 like any unrouted request.
	if gjson.GetBytes(in, "_txc.src").String() == "room" {
		if rt := gjson.GetBytes(in, "_txc.room.tenant").String(); rt != "" {
			raw := "{}"
			raw, _ = sjson.Set(raw, "_txc.route.tenant", rt)
			raw, _ = sjson.Set(raw, "_txc.route.stack", "_room")
			raw, _ = sjson.Set(raw, "_txc.route.ingress", "room")
			raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", true)
			raw, _ = sjson.Set(raw, "_txc.route.to", "_room/0")
			return raw
		}
	}
	// Inspect request. The inspect inlet (admin POST /v1/tenants/{t}/inspect)
	// stamps the target slug in `_txc.inspect.tenant` (trusted: the
	// authenticated URL tenant, never client input). Propose a route into that
	// tenant's `_inspect/0` — the same sanctioned _sys→tenant pin as room. A
	// tenant with no `_inspect` stack falls through to the 404 like any
	// unrouted request; the inlet reports that as "no inspector answered".
	if gjson.GetBytes(in, "_txc.src").String() == "inspect" {
		if it := gjson.GetBytes(in, "_txc.inspect.tenant").String(); it != "" {
			raw := "{}"
			raw, _ = sjson.Set(raw, "_txc.route.tenant", it)
			raw, _ = sjson.Set(raw, "_txc.route.stack", "_inspect")
			raw, _ = sjson.Set(raw, "_txc.route.ingress", "inspect")
			raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", true)
			raw, _ = sjson.Set(raw, "_txc.route.to", "_inspect/0")
			return raw
		}
	}
	// Scheduled event. The scheduled personality fires a claimed due event,
	// stamping the target slug in `_txc.scheduled.tenant` (trusted: from the
	// stored row's tenant, pinned at enqueue from processor.TenantScope, never
	// client input). Propose a route into that tenant's `_scheduled/0` — the
	// same sanctioned _sys→tenant pin as cron/room. The tenant's stack reads
	// the stored job off `@scheduled.payload.*`.
	if gjson.GetBytes(in, "_txc.src").String() == "scheduled" {
		if st := gjson.GetBytes(in, "_txc.scheduled.tenant").String(); st != "" {
			raw := "{}"
			raw, _ = sjson.Set(raw, "_txc.route.tenant", st)
			raw, _ = sjson.Set(raw, "_txc.route.stack", "_scheduled")
			raw, _ = sjson.Set(raw, "_txc.route.ingress", "scheduled")
			raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", true)
			raw, _ = sjson.Set(raw, "_txc.route.to", "_scheduled/0")
			return raw
		}
	}
	target, hit := resolver.Resolve(ingress.KeyFromEnvelope(string(in)))
	if !hit {
		return "{}"
	}
	raw := "{}"
	raw, _ = sjson.Set(raw, "_txc.route.tenant", target.Tenant)
	raw, _ = sjson.Set(raw, "_txc.route.stack", target.Stack)
	raw, _ = sjson.Set(raw, "_txc.route.ingress", target.Ingress)
	raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", target.Verified)
	raw, _ = sjson.Set(raw, "_txc.route.to", target.Stack+"/0")
	return raw
}

// routeBody is the txco://route transform — the EXECUTE half of the
// boot pipeline (scope 100). It is pure mechanism: promote the inert
// `_txc.route.*` proposal into the active control keys the processor
// acts on. The route-or-not *decision* lives in the visible
// `WHEN @route.to != ""` gate on the boot/100 rule, not here — this
// op only runs once that gate has fired. The defensive empty-route
// return keeps it safe even if an operator edits the WHEN away.
//
// Faithful parity: emits exactly the `_txc.*` keys detect-tenant used
// to set directly, so tenant stacks and the trace see an unchanged
// envelope (modulo the intended extra boot/100 step). `_txc.tenant` is
// emitted only when the proposal carries one — the continuation route
// has none, so the pin stays `_sys` and txc-continuation/0 resolves
// under `_sys`. Parity keys are carried only when present in the
// proposal (continuation polls get just the goto, as before).
//
// The spent `_txc.route.*` proposal is NOT removed here (op output is
// merged, which can only add) — the chassis strips it when this op's
// `_txc.goto` is consumed (processor.advanceAfterScope, alongside the
// halt/goto strip), so the promoted keys above are the single surviving
// copy rather than a duplicate riding next to the inert proposal.
func routeBody(in []byte) string {
	to := gjson.GetBytes(in, "_txc.route.to").String()
	if to == "" {
		return "{}"
	}
	raw := "{}"
	raw, _ = sjson.Set(raw, "_txc.goto", to)
	// Tenant is ESTABLISHED once — at the _sys→concrete boot handoff — and may
	// only be re-affirmed afterward. A (tenant) op can re-run route to re-pin the
	// STACK for cross-stack dispatch, but it must NOT be able to SWITCH the tenant
	// via `SET @route.tenant=other`. route is the only path that can set
	// `_txc.tenant` (EMIT + untrusted op output are blocked).
	//
	// The "unestablished" current value is EITHER empty OR the system tenant:
	// dispatchEnvelope stamps `_txc.tenant=_sys` on every request before the boot
	// pipeline, so at boot/100 `cur` is "_sys", not "". Promoting from "" or `_sys`
	// is the one-way _sys→concrete handoff maybeRetenant relies on; a CONCRETE
	// `cur` that differs from the proposal is a switch attempt, so it's omitted and
	// the per-scope merge keeps the established value. (Allowing only cur=="" here
	// silently dropped the boot promotion and broke ALL tenant routing.)
	if tn := gjson.GetBytes(in, "_txc.route.tenant"); tn.Exists() && tn.String() != "" {
		if cur := gjson.GetBytes(in, "_txc.tenant").String(); cur == "" || cur == tenants.SystemTenantSlug || cur == tn.String() {
			raw, _ = sjson.Set(raw, "_txc.tenant", tn.String())
		}
	}
	if s := gjson.GetBytes(in, "_txc.route.stack"); s.Exists() {
		raw, _ = sjson.Set(raw, "_txc.stack", s.String())
	}
	if ig := gjson.GetBytes(in, "_txc.route.ingress"); ig.Exists() {
		raw, _ = sjson.Set(raw, "_txc.ingress", ig.String())
	}
	if hv := gjson.GetBytes(in, "_txc.route.hostname_verified"); hv.Exists() {
		raw, _ = sjson.Set(raw, "_txc.hostname_verified", hv.Bool())
	}
	return raw
}

// continuationResultBody is the txco://continuation-result transform: a
// poll/deferred-response for an existing run, reached via the
// `?_txc.continuation=<rcid>` short-circuit in detectTenantBody. It reads
// the run ONLY by the opaque rcid (a 128-bit capability — possession is
// authorization, the same trust model as the removed dedicated GET); no
// worker- or hostname-supplied tenant influences it. It returns an
// envelope shaped for the normal web response path (_txc.web.res.*):
// completed → the stored result's web response (the deferred page), or
// the JSON status doc when ?format=json; waiting/resumable → 202 +
// Retry-After; failed → 502; unknown → 404.
func continuationResultBody(ctx context.Context, runs *continuation.Runs, longPollMS int, in []byte) string {
	resp := func(status int, retryAfter, bodyJSON string) string {
		env := "{}"
		env, _ = sjson.Set(env, "_txc.web.res.status", status)
		env, _ = sjson.Set(env, "_txc.web.res.headers.content-type.0", "application/json")
		// The poll URL changes over time (202/404/502/200); never let a
		// shared or browser cache pin a transient answer.
		env, _ = sjson.Set(env, "_txc.web.res.headers.cache-control.0", "no-store")
		if retryAfter != "" {
			env, _ = sjson.Set(env, "_txc.web.res.headers.retry-after.0", retryAfter)
		}
		env, _ = sjson.Set(env, "_txc.web.res.body",
			base64.StdEncoding.EncodeToString([]byte(bodyJSON)))
		return env
	}
	statusDoc := func(rcid, state string) string {
		d := "{}"
		d, _ = sjson.Set(d, "continuation", rcid)
		d, _ = sjson.Set(d, "status", state)
		return d
	}

	rcid := gjson.GetBytes(in, "_txc.continuation").String()
	format := gjson.GetBytes(in, "_txc.web.req.url.query.format.0").String()
	if runs == nil || rcid == "" {
		return resp(404, "", `{"error":"unknown_continuation","reason":"empty_or_missing_rcid"}`)
	}

	runID, err := runs.ResolveRunContinuation(ctx, rcid)
	if err == continuation.ErrNotFound {
		// Two ways to land here: the rcid was never minted (typo,
		// stale link, bot probe) OR the run completed and the sweep
		// purged it past retention (continuation/sweep). We can't
		// distinguish them from disk state alone — the purge deletes
		// the rcid→runID lookup unconditionally — but we can give
		// the caller the rcid back + a hint they can grep with.
		hint := "unknown_or_expired"
		if !strings.HasPrefix(rcid, "rc_") {
			// Caller-side typo: real rcids always start with rc_.
			// Flag this distinctly so log grep separates malformed
			// inputs from purged runs.
			hint = "malformed_rcid"
		}
		bodyJSON, _ := json.Marshal(map[string]string{
			"error":  "unknown_continuation",
			"reason": hint,
			"rcid":   rcid,
		})
		return resp(404, "", string(bodyJSON))
	}
	if err != nil {
		return resp(500, "", `{"error":"lookup_failed"}`)
	}
	state, err := runs.RunState(ctx, runID)
	if err != nil {
		return resp(500, "", `{"error":"state failed"}`)
	}

	// Adaptive long-poll: rather than answering 202 immediately and
	// letting the client re-poll on a fixed grid, hold the JSON poll
	// open and re-derive state until it goes terminal or the budget
	// runs out. Browsers asking for the HTML wait page still get it
	// instantly — they only reach here via ?format=json. The wait is
	// bounded by the request deadline (the web inlet caps the request
	// at WebWriteTimeout), so a still-waiting run falls through to the
	// same 202 as before; longPollMS == 0 restores single-shot polls.
	if longPollMS > 0 && format == "json" &&
		(state == continuation.StateWaiting || state == continuation.StateResumable) {
		state = awaitTerminalState(ctx, runs, runID, state, longPollMS)
	}

	switch state {
	case continuation.StateCompleted:
		res, ok, _ := runs.ReadResult(ctx, runID)
		if !ok {
			return resp(500, "", `{"error":"result missing"}`)
		}
		if format == "json" {
			d := "{}"
			d, _ = sjson.Set(d, "continuation", rcid)
			d, _ = sjson.Set(d, "status", "completed")
			d, _ = sjson.SetRaw(d, "result", string(res))
			return resp(200, "", d)
		}
		// Deferred page: surface ONLY the stored result's web response
		// (status/headers/base64 body) — not the whole stored envelope —
		// so another run's fields/tenant never merge into this poll.
		if wr := gjson.GetBytes(res, "_txc.web.res"); wr.Exists() {
			env := "{}"
			env, _ = sjson.SetRaw(env, "_txc.web.res", wr.Raw)
			// The result is fetched via an unguessable one-shot poll
			// handle, not a stable resource — keep it out of shared
			// caches. Set-if-absent: if the app's pipeline deliberately
			// set its own Cache-Control, that intent wins.
			if !gjson.Get(env, "_txc.web.res.headers.cache-control.0").Exists() {
				env, _ = sjson.Set(env, "_txc.web.res.headers.cache-control.0", "no-store")
			}
			return env
		}
		// Completed run with no web response (non-HTTP result): return
		// the raw result as the JSON body.
		return resp(200, "", string(res))
	case continuation.StateWaiting, continuation.StateResumable:
		// Content negotiation: a browser (Accept: text/html, not
		// ?format=json) gets the branded Svelte waiting page; everything
		// else (curl, the page's own fetch poll, ?format=json) gets the
		// JSON 202. No `Refresh` response header: the page owns refresh
		// (smooth fetch poll + a <noscript> meta-refresh fallback); a
		// server Refresh header would hard-reload and fight the JS
		// poller. Retry-After stays (semantic; harmless to browsers).
		accept := gjson.GetBytes(in, "_txc.web.req.headers.Accept.0").String()
		if format != "json" && strings.Contains(accept, "text/html") {
			page, _ := continuationui.WaitPage()
			env := "{}"
			env, _ = sjson.Set(env, "_txc.web.res.status", 202)
			env, _ = sjson.Set(env, "_txc.web.res.headers.content-type.0", "text/html; charset=utf-8")
			env, _ = sjson.Set(env, "_txc.web.res.headers.cache-control.0", "no-store")
			env, _ = sjson.Set(env, "_txc.web.res.headers.retry-after.0", "3")
			env, _ = sjson.Set(env, "_txc.web.res.body", base64.StdEncoding.EncodeToString(page))
			return env
		}
		return resp(202, "3", statusDoc(rcid, state))
	case continuation.StateFailed:
		return resp(502, "", statusDoc(rcid, "failed"))
	default:
		return resp(500, "", `{"error":"unknown state"}`)
	}
}

// awaitTerminalState holds a continuation JSON poll open, re-deriving
// run state on a ~1s cadence until it reaches a terminal state
// (completed/failed) or the budget expires. The budget is the smaller
// of the configured cap and the request's own deadline minus 1.5s, so
// we always return in time for the caller to emit a clean 202 before
// the web inlet's WebWriteTimeout kills the request context. ctx
// cancellation (client disconnect / shutdown) returns immediately. On
// a transient RunState error it returns the last known state so the
// caller falls through to the normal 202 and the client re-polls.
func awaitTerminalState(ctx context.Context, runs *continuation.Runs, runID, state string, maxMS int) string {
	budget := time.Duration(maxMS) * time.Millisecond
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl) - 1500*time.Millisecond; d < budget {
			budget = d
		}
	}
	if budget <= 0 {
		return state
	}

	deadline := time.After(budget)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return state
		case <-deadline:
			return state
		case <-ticker.C:
			s, err := runs.RunState(ctx, runID)
			if err != nil {
				return state
			}
			state = s
			if state == continuation.StateCompleted || state == continuation.StateFailed {
				return state
			}
		}
	}
}

// staticResultBody is the txco://static transform: pure mechanism, and
// pure in-memory — the Index holds the bytes, so this never touches the
// filesystem on the request path. boot/50 EXECs it unconditionally; the
// gate IS the index. It resolves the request path layered against the
// routed stack's FILES (the inert _txc.route.stack proposal from
// detect-tenant@0, available at scope 50), then chassis-wide, then the
// embedded default.
//
//   - exact file → 200 (+ strong ETag, or 304 on If-None-Match match),
//     halt;
//   - no file but under a static-owned directory prefix (e.g. an
//     `assets/` dir exists) → 404, halt — static owns that subtree, the
//     request must NOT fall through to the app;
//   - otherwise → "{}" (no _txc.web.res, no halt) so the request keeps
//     flowing through scope 100 (route) / 1000 (404) as before.
func staticResultBody(ctx context.Context, ix *static.Index, fcas filecas.Store, in []byte) string {
	reqPath := gjson.GetBytes(in, "_txc.web.req.url.path").String()
	stack := gjson.GetBytes(in, "_txc.route.stack").String()
	tenant := gjson.GetBytes(in, "_txc.route.tenant").String()

	// Privacy convention: any request-path segment beginning with "_" is a
	// private asset (e.g. FILES/_mail/ templates) — indexed and readable by
	// ops, but never served over HTTP. Fall through "{}" (NOT 404) so its
	// existence doesn't leak. Runs before Lookup, so the Owned-prefix branch
	// can't leak either. path.Clean resolves "/a/../_x" to its real segments.
	for _, seg := range strings.Split(strings.Trim(path.Clean("/"+reqPath), "/"), "/") {
		if seg != "" && seg[0] == '_' {
			return "{}"
		}
	}

	r := ix.Lookup(tenant, stack, reqPath)

	// Terminal helper: every static answer halts the pipeline (same
	// mechanism _sys/boot/1000/notfound.txcl relies on).
	halt := func(env string) string {
		env, _ = sjson.Set(env, "_txc.halt", true)
		return env
	}

	// Cache policy: HTML is the entry point that references content-hashed assets, so
	// it must revalidate — otherwise a deploy's new HTML (and thus the new asset URLs)
	// isn't picked up until the cache lapses. `max-age=0, must-revalidate` makes the
	// browser check every time; the strong ETag makes that check a cheap 304. Other
	// assets (typically content-hashed, immutable) stay cacheable.
	cacheControl := "public, max-age=3600"
	if strings.HasPrefix(r.Ctype, "text/html") {
		cacheControl = "max-age=0, must-revalidate"
	}

	// Conditional GET applies to any Found result (inline or CAS) and is
	// checked before fetching bytes — a 304 never touches the CAS/LRU.
	if r.Found {
		if inm := gjson.GetBytes(in, "_txc.web.req.headers.If-None-Match.0").String(); inm != "" &&
			(inm == r.ETag || inm == "*") {
			env := "{}"
			env, _ = sjson.Set(env, "_txc.web.res.status", 304)
			env, _ = sjson.Set(env, "_txc.web.res.headers.etag.0", r.ETag)
			env, _ = sjson.Set(env, "_txc.web.res.headers.cache-control.0", cacheControl)
			return halt(env)
		}
	}

	serve := func(body []byte) string {
		env := "{}"
		env, _ = sjson.Set(env, "_txc.web.res.status", 200)
		env, _ = sjson.Set(env, "_txc.web.res.headers.content-type.0", r.Ctype)
		env, _ = sjson.Set(env, "_txc.web.res.headers.cache-control.0", cacheControl)
		env, _ = sjson.Set(env, "_txc.web.res.headers.etag.0", r.ETag)
		env, _ = sjson.Set(env, "_txc.web.res.body", base64.StdEncoding.EncodeToString(body))
		return halt(env)
	}

	switch {
	case r.Found && r.Hash != "":
		// Tenant CAS entry: resolve bytes lazily by content hash. A nil store
		// or a missing/errored object falls through "{}" rather than hard-
		// failing the page — the metadata index can briefly lead the CAS.
		if fcas == nil {
			return "{}"
		}
		body, err := fcas.Get(ctx, r.Hash)
		if err != nil {
			return "{}"
		}
		return serve(body)
	case r.Found:
		// Inline (operator) layer — bytes are already in the index.
		return serve(r.Body)
	case r.Owned:
		// The directory prefix is static's; don't let the app see it.
		env := "{}"
		env, _ = sjson.Set(env, "_txc.web.res.status", 404)
		env, _ = sjson.Set(env, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
		env, _ = sjson.Set(env, "_txc.web.res.body",
			base64.StdEncoding.EncodeToString([]byte("404 not found\n")))
		return halt(env)
	default:
		return "{}"
	}
}

func dispatchEnvelope(raw, missAction string) (string, string) {
	// reject mode hard-404s unrouted ingress without the boot pipeline —
	// but only for HTTP. A non-HTTP source (the cron system tick, tcp,
	// lmtp) has no business getting a web 404, so it falls through into the
	// boot pipeline (whose scope-1000 notfound is likewise HTTP-gated) and
	// halts quietly instead of emitting a web-shaped 404 for a non-web
	// transport.
	if missAction == "reject" && gjson.Get(raw, "_txc.src").String() == "http" {
		return raw, stageNoRoute
	}
	raw = ingress.StampEnvelope(raw, ingress.RouteTarget{
		Tenant:   tenants.SystemTenantSlug,
		Stack:    "boot",
		Ingress:  "ingress",
		Verified: false,
	})
	return raw, defaultEntryStage
}

// emitNoRouteResponse writes a synthetic HTTP 404 response directly
// to the envelope's ResCh, bypassing the processor. The envelope is
// shaped to match what stack-emitted responses look like
// (_txc.web.res.status, _txc.web.res.headers.content-type.0,
// _txc.web.res.body) so the web inlet's response-writer renders it
// the same way it renders any other response — see
// chassis/server/personality/web/response.go.
//
// The client-facing body is intentionally minimal — same shape as
// `http.NotFound`. Internal context (the request's Host header, the
// chassis flag, etc.) goes to the chassis log via the caller, not
// the response, so we don't leak routing config to arbitrary HTTP
// clients.
//
// Used when `--ingress-miss-action=reject` is set and no
// tenant_hostnames row matches the incoming request.
func emitNoRouteResponse(envelope *event.Envelope) {
	raw := envelope.Payload.Raw
	if raw == "" {
		raw = "{}"
	}
	raw, _ = sjson.Set(raw, "_txc.web.res.status", 404)
	raw, _ = sjson.Set(raw, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
	raw, _ = sjson.Set(raw, "_txc.web.res.body",
		base64.StdEncoding.EncodeToString([]byte("404 not found\n")))

	select {
	case envelope.ResCh <- event.Payload{Raw: raw, Type: event.JSON}:
	case <-envelope.Ctx.Done():
		// Caller already gave up; drop the synthetic response.
	}
}

// emitDrainResponse answers an envelope with a 503 "draining" response
// while the node is bleeding out of its load balancer (SIGUSR1). Like
// emitNoRouteResponse it bypasses the processor and shapes the envelope
// so the web inlet renders it the same way it renders any stack-emitted
// response (admission.DrainResponse sets _txc.web.res.* + Retry-After).
func emitDrainResponse(envelope *event.Envelope) {
	raw := envelope.Payload.Raw
	if raw == "" {
		raw = "{}"
	}
	raw = admission.DrainResponse(raw)
	select {
	case envelope.ResCh <- event.Payload{Raw: raw, Type: event.JSON}:
	case <-envelope.Ctx.Done():
		// Caller already gave up; drop the synthetic response.
	}
}

// runPipeline dispatches the processor and, when capture is set, tees
// the final response so the caller learns the response bytes (for
// usage sizing + the resolved `_txc.tenant`, which the pipeline pins in
// context and never writes back to the inlet envelope). The tee adds a
// goroutine + buffered channel per request; when neither tracing nor
// usage needs the payload (capture=false, the production default with
// both off) we skip it entirely and hand the inlet's ResCh straight to
// the processor — zero overhead, the original behavior.
func runPipeline(
	ctx context.Context,
	pu *processor.Unit,
	envelope *event.Envelope,
	raw, stage string,
	capture bool,
) (finalPayload []byte, fuelUsed int64, err error) {
	if !capture {
		return nil, 0, pu.Run(ctx, raw, stage, envelope.ResCh)
	}

	teeCh := make(chan event.Payload, 1)
	teeDone := make(chan struct{})
	go func() {
		defer close(teeDone)
		// Forward every payload from the processor to the inlet's ResCh
		// until teeCh closes. A non-streaming request sends exactly one
		// payload (one iteration, then close) — behavior-identical to the
		// previous single-receive form. A streamed response sends
		// StreamHead + N×StreamChunk + StreamEnd; we relay them all and
		// capture the head (or the lone JSON payload) as finalPayload for
		// usage sizing + the resolved _txc.tenant. Raw chunk bytes are not
		// captured (streamed-body sizing is out of scope for v0 usage).
		for {
			select {
			case p, ok := <-teeCh:
				if !ok {
					return
				}
				if p.Type != event.StreamChunk {
					// Read fuel BEFORE strip so the meter survives the
					// cleanup, then strip the chassis-internal budget
					// fields so the client never sees them. finalPayload
					// captures the post-strip bytes so BytesOut on the
					// usage line reflects what the inlet actually wrote.
					fuelUsed = processor.FuelUsedFromEnvelope(p.Raw)
					p.Raw = processor.StripBudgetFromOutbound(p.Raw)
					finalPayload = []byte(p.Raw)
				}
				select {
				case envelope.ResCh <- p:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	err = pu.Run(ctx, raw, stage, teeCh)
	close(teeCh)
	<-teeDone
	return finalPayload, fuelUsed, err
}

// runWithTrace dispatches the processor through runPipeline and, when
// tracing is enabled, records the request to the trace sink. The tee
// now lives in runPipeline rather than here: usage logging and tenant
// telemetry need the final payload in production where the trace sink
// is NoopSink, so the capture decision is `tracing OR usage OR
// telemetry`, not `tracing` alone. The captured final payload is
// returned so the bus loop can size the response, read the resolved
// tenant for the usage event, and hand metric intents to the telemetry
// processor.
func runWithTrace(
	ctx context.Context,
	pu *processor.Unit,
	sink trace.Sink,
	envelope *event.Envelope,
	raw, stage string,
	usageEnabled bool,
	telemetryEnabled bool,
) (finalPayload []byte, fuelUsed int64, err error) {
	_, isNoop := sink.(trace.NoopSink)
	capture := !isNoop || usageEnabled || telemetryEnabled

	if isNoop {
		return runPipeline(ctx, pu, envelope, raw, stage, capture)
	}

	tracer := sink.Begin(trace.RequestInfo{
		RID:       envelope.Rid,
		Src:       envelope.Src,
		Tenant:    gjson.Get(raw, "_txc.tenant").String(),
		Stack:     stage,
		StartedAt: time.Now(),
		Payload:   []byte(raw),
	})
	ctx = trace.WithContext(ctx, tracer)

	finalPayload, fuelUsed, err = runPipeline(ctx, pu, envelope, raw, stage, capture)

	status := "ok"
	reason := ""
	if err != nil {
		status = "error"
		// The pipeline error is the request-level "why". processor.Run
		// enriches the abandon/halt error with the stalled op (e.g.
		// "canceled while running test-stack/50 mcp+https://…"), so this
		// one line answers "why is it an error?" at the top of the trace.
		reason = err.Error()
	}
	// Surface the per-request usage primitives (fuel, response size, resolved
	// tenant) into the trace via the shared request.usage event so the admin
	// detail view + tenant-scoping read them. Uses the authoritative fuelUsed
	// return value (not the strippable _txc.fuel_used); the resolved tenant
	// (not the `_sys` entry tenant) comes from the final envelope. Rides the
	// generic timeline Event, written in every mode.
	trace.EmitUsage(tracer, fuelUsed, len(finalPayload), processor.TenantFromEnvelope(string(finalPayload)))
	tracer.End(status, reason, finalPayload)
	return finalPayload, fuelUsed, err
}

type controller interface {
	Start()
	Stop()
}

func Start(ctx context.Context, conf config.Config, logger *zap.Logger, kv store.Store, runtimeDB, authDB *sql.DB, dbc *dbcache.DbCache, secretsResolver *secrets.Resolver, scheduledStore *scheduled.Store) (modCtx context.Context, stop func(reason string), err error) {

	ctx, cancel := context.WithCancel(ctx)

	// Setup Metrics Collection
	mc := metrics.New(ctx, conf, logger)

	// Setup Registry
	reg := registry.New(conf, logger)

	// Load the ingress resolver. YAML entries always win for backward
	// compatibility; on a miss the DB resolver is consulted against the
	// dbcache mirror (hostname → (tenant_slug, stack) via
	// tenant_hostnames). A configured-but-broken YAML is a startup
	// error — fail loudly rather than silently dropping ingress
	// routing.
	yamlResolver, err := ingress.LoadResolverFromFile(
		conf.IngressConfigPath,
		ingress.WithDefaultMailHosts(conf.LMTPDefaultHosts),
	)
	if err != nil {
		cancel()
		return ctx, nil, err
	}
	if yamlResolver != nil {
		logger.Info("ingress router loaded", zap.String("path", conf.IngressConfigPath))
	}
	// dbc.Snapshot (not dbc.Db): Reload() swaps the mirror handle, so a
	// captured pointer would only ever see boot-time hostname rows —
	// post-boot binds/mints (operator or auto-minted) would 404 until
	// restart. Snapshot returns the live handle per request.
	resolver := ingress.NewDBResolverFunc(yamlResolver, dbc.Snapshot, logger, conf.RequireHostnameVerification)

	// Redact/omit registry: per-(tenant, stack) lists harvested from
	// every rule's WITH clause. The trace sink consults it on the
	// worker thread to mask or delete configured paths before they
	// land on disk. Built once at startup; rebuilt on every dbcache
	// reload via the OnReload chain below (so `txco apply` picks up
	// hint changes without a chassis restart).
	redactReg := newRedactRegistry(logger)
	if dbc != nil {
		if rerr := redactReg.Rebuild(dbc.Snapshot()); rerr != nil {
			logger.Warn("redact registry initial build failed",
				zap.String("err", rerr.Error()))
		}
		prevOnReload := dbc.OnReload
		dbc.OnReload = func(db *sql.DB) error {
			var perr error
			if prevOnReload != nil {
				perr = prevOnReload(db)
			}
			if rerr := redactReg.Rebuild(db); rerr != nil {
				logger.Warn("redact registry reload failed",
					zap.String("err", rerr.Error()))
			}
			return perr
		}
	}

	// Per-tenant admission state (tenant_runtime_state): an in-memory
	// snapshot read on the request path, rebuilt on every dbcache reload
	// via the OnReload chain (so an operator edit or a fleet-sync
	// entitlement.updated apply takes effect without a restart). Inert
	// until a row denies a tenant — a tenant with no row is admitted.
	admissionProv := admission.NewSQLiteProvider(logger)
	if dbc != nil {
		if rerr := admissionProv.Rebuild(dbc.Snapshot()); rerr != nil {
			logger.Warn("admission provider initial build failed",
				zap.String("err", rerr.Error()))
		}
		prevOnReload := dbc.OnReload
		dbc.OnReload = func(db *sql.DB) error {
			var perr error
			if prevOnReload != nil {
				perr = prevOnReload(db)
			}
			if rerr := admissionProv.Rebuild(db); rerr != nil {
				logger.Warn("admission provider reload failed",
					zap.String("err", rerr.Error()))
			}
			return perr
		}
	}

	// Load the trace sink. NoopSink (mode=off) is the zero-cost default.
	// FileSink writes the per-request artifact tree; failure to create
	// the trace dir is a startup error. When --trace-async is set, the
	// FileSink gets wrapped so the request path doesn't block on disk
	// I/O — events queue through a worker goroutine that drains on
	// shutdown.
	//
	// Redaction is applied BELOW the AsyncSink layer (so it runs on
	// the async worker, off the request hot path) and ABOVE the file
	// sink (so the masked bytes are what hit disk). When async is off,
	// redaction wraps the file sink directly and runs on the request
	// goroutine just before the disk write.
	var traceSink trace.Sink = trace.NoopSink{}
	if mode := trace.ParseMode(conf.TraceMode); mode != trace.ModeOff {
		// mode!=off ⇒ build the configured backend (default "file"; an
		// out-of-tree shipper self-registers and is selected by
		// --trace-store). mode=off keeps the zero-cost NoopSink and
		// constructs nothing.
		base, berr := trace.Open(conf.TraceStore, trace.StoreConfig{Dir: conf.TraceDir, Mode: mode})
		if berr != nil {
			cancel()
			return ctx, nil, berr
		}
		base = trace.NewRedactingSink(base, redactReg.Hints)
		if conf.TraceAsync {
			traceSink = trace.NewAsyncSink(base, trace.AsyncOpts{
				BufferSize:   conf.TraceBufferSize,
				BodyCapBytes: conf.TraceBodyCapBytes,
			})
			logger.Info("trace sink loaded",
				zap.String("store", conf.TraceStore),
				zap.String("dir", conf.TraceDir),
				zap.String("mode", string(mode)),
				zap.Bool("async", true),
				zap.Int("buffer_size", conf.TraceBufferSize),
				zap.Int("body_cap_bytes", conf.TraceBodyCapBytes))
		} else {
			traceSink = base
			logger.Info("trace sink loaded",
				zap.String("store", conf.TraceStore),
				zap.String("dir", conf.TraceDir),
				zap.String("mode", string(mode)),
				zap.Bool("async", false))
		}
	}

	// Usage sink. On by default — every completed request is folded
	// into a structured "usage" log line through the existing logger
	// (the bundled default); --usage-enabled=false opts out for the
	// zero-cost path (nil sink, no response tee). The Sink interface
	// lets a file/Kafka/OTEL transport drop in later without touching
	// the bus loop.
	var usageSink usage.Sink
	if conf.UsageEnabled {
		// Epoch is a dedicated per-boot mint, NOT conf.ServerId: an
		// out-of-tree cumulative sink keys its counters on the epoch as a
		// reset boundary, and conf.ServerId (--sid) is a pinnable knob, so
		// reusing it could collide a fresh-from-zero local store with a
		// prior cumulative and undercount. A fresh mint per boot guarantees
		// a new series every boot. sid is logged for correlation.
		usageEpoch := hxid.New().String()
		s, uerr := usage.Open(conf.UsageSink, usage.SinkConfig{
			Epoch:   usageEpoch,
			NodeID:  resolveUsageNodeID(conf.Fqdn),
			DataDir: conf.DbRoot,
			Logger:  logger,
		})
		if uerr != nil {
			cancel()
			return ctx, nil, uerr
		}
		usageSink = s
		logger.Info("usage sink loaded",
			zap.String("sink", usageSink.Name()),
			zap.String("epoch", usageEpoch),
			zap.String("sid", conf.ServerId))
	}

	// Continuation store: durable, immutable, derived-state storage for
	// suspended (async) runs. The file backend is the bundled default
	// and self-registers via the blank import. Construction failure is a
	// startup error. Only the barrier (async) path touches it; the sync
	// fast path never does.
	cstore, cerr := continuation.Open(conf.ContinuationStore, continuation.StoreConfig{
		FileDir:  conf.ContinuationStoreFileDir,
		S3Bucket: conf.ContinuationStoreS3Bucket,
		S3Prefix: conf.ContinuationStoreS3Prefix,
	})
	if cerr != nil {
		cancel()
		return ctx, nil, cerr
	}
	runs := continuation.NewRuns(cstore)
	logger.Info("continuation store loaded",
		zap.String("backend", cstore.Name()),
		zap.String("dir", conf.ContinuationStoreFileDir))

	// Artifact store: content-addressed home for snapshot/event artifacts
	// (the bytes a control event references; see
	// internal docs/todo-architecture-saas-fleet.md §3.1). The file backend
	// self-registers via the blank import. The serving path never reads
	// it; it backs the snapshot bootstrap (CLI / --snapshot-bootstrap-ref)
	// and the control-event applier.
	astore, aerr := artifact.Open(conf.ArtifactStore, artifact.StoreConfig{
		FileDir: conf.ArtifactStoreFileDir,
	})
	if aerr != nil {
		cancel()
		return ctx, nil, aerr
	}
	logger.Info("artifact store loaded",
		zap.String("backend", astore.Name()),
		zap.String("dir", conf.ArtifactStoreFileDir))

	// filecas: content-addressed store for tenant FILES/ assets. The file
	// backend self-registers via the blank import; the fleet uses the S3
	// overlay backend. txco://static resolves tenant file bytes through it
	// (lazily, behind an LRU); the metadata (path → hash) lives in the
	// static index. Empty FileCASStore ⇒ nil ⇒ tenant assets fall through.
	var fcas filecas.Store
	if conf.FileCASStore != "" {
		var fcerr error
		fcas, fcerr = filecas.Open(conf.FileCASStore, filecas.StoreConfig{
			FileDir:       conf.FileCASStoreFileDir,
			S3Bucket:      conf.FileCASStoreS3Bucket,
			S3Prefix:      conf.FileCASStoreS3Prefix,
			CacheBytes:    int64(conf.FileCASCacheBytes),
			MaxEntryBytes: int64(conf.FileCASMaxFileBytes),
		})
		if fcerr != nil {
			cancel()
			return ctx, nil, fcerr
		}
		logger.Info("filecas store loaded",
			zap.String("backend", fcas.Name()),
			zap.String("dir", conf.FileCASStoreFileDir))
	}

	// Dataset cache: node-local materialise + read-only handle cache for
	// DATASETS/ artifacts (chassis/dataset), resolving misses from filecas.
	// Powers the txco://dataset op, the deep activation gate, and the fleet
	// applier's post-commit warm. Requires a filecas store; without one the
	// op stays unregistered and dataset-bearing versions refuse activation.
	var dsCache *dataset.Cache
	if fcas != nil {
		var dcErr error
		dsCache, dcErr = dataset.NewCache(conf.DatasetCacheDir, fcas, int64(conf.DatasetCacheBytes))
		if dcErr != nil {
			logger.Error("dataset cache init failed; txco://dataset disabled",
				zap.String("dir", conf.DatasetCacheDir), zap.Error(dcErr))
		} else {
			logger.Info("dataset cache ready",
				zap.String("dir", conf.DatasetCacheDir),
				zap.Int("budget_bytes", conf.DatasetCacheBytes))
		}
	}

	// Control-event feed source. Default "nop" yields nothing, so the
	// applier controller is inert and single-node behaviour is unchanged.
	fsrc, ferr := feed.Open(conf.FeedSource, feed.SourceConfig{
		FileDir: conf.FeedSourceFileDir,
	})
	if ferr != nil {
		cancel()
		return ctx, nil, ferr
	}
	logger.Info("control-event feed source loaded",
		zap.String("source", fsrc.Name()))

	// Control-event feed sink (producer side). Default "nop" discards;
	// the pump checks --feed-sink != nop and skips starting if so.
	// Single-node behaviour is unchanged.
	fsink, sinkErr := feed.OpenSink(conf.FeedSink, feed.SourceConfig{
		FileDir: conf.FeedSourceFileDir,
	})
	if sinkErr != nil {
		cancel()
		return ctx, nil, sinkErr
	}
	logger.Info("control-event feed sink loaded",
		zap.String("sink", fsink.Name()))

	// Cron dispatch queue. Default "local" is the in-process channel +
	// worker pool, so single-node behaviour is unchanged. An overlay
	// registers e.g. "nats" via blank import and is selected with
	// --cron-queue.
	//
	// A queue that can't be constructed (unknown backend, or a broker
	// backend missing its auth/URL) must NOT take down the chassis: cron is
	// a non-critical, opt-in personality, and the data plane / admin / web
	// heads have nothing to do with it. Log loudly and run with cron
	// DISABLED (nil queue → the controller no-ops). We deliberately do NOT
	// fall back to the "local" queue, because on a fleet that would
	// reintroduce duplicate firing. Fix the config and restart to enable.
	cq, cqErr := cronq.Open(conf.CronQueue, cronq.Config{
		MaxInflight: conf.CronMaxInflight,
		Period:      conf.CronPeriod,
	})
	if cqErr != nil {
		logger.Error("cron queue failed to load; cron disabled (data plane unaffected)",
			zap.String("queue", conf.CronQueue), zap.Error(cqErr))
		cq = nil
	} else {
		logger.Info("cron queue loaded", zap.String("queue", cq.Name()))
	}

	// Outbound op-dial policy. Default "open" allows everything, so
	// local/test behaviour is unchanged; "private" refuses dials into
	// loopback/private/internal address space. A bad CIDR fails loudly
	// here at boot rather than at first dial.
	guard, gerr := egress.Open(conf.EgressPolicy, egress.Config{
		DenyCIDRs:  conf.EgressDenyCIDRs,
		AllowCIDRs: conf.EgressAllowCIDRs,
	})
	if gerr != nil {
		cancel()
		return ctx, nil, gerr
	}
	logger.Info("egress policy loaded",
		zap.String("policy", guard.Name()))

	// Tenant telemetry: the request-end processor that turns
	// _txc.telemetry.metrics intents into exports. A tenant goes live by
	// setting its TELEMETRY_ENDPOINT secret; the node just provides the
	// machinery. The exporter gets its OWN egress-guarded client (same
	// DialControl as op dials, no otelhttp wrap — the chassis shouldn't
	// trace its own telemetry egress) because the destination is
	// tenant-supplied.
	var telemetryProc *telemetry.Processor
	if conf.TelemetryEnabled {
		tTransport := http.DefaultTransport.(*http.Transport).Clone()
		tTransport.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   egress.DialControl(guard),
		}).DialContext
		dropped := func(tenant, reason string, n int64) {
			mc.RecordTelemetryDrop(context.Background(), tenant, reason, n)
		}
		texp, terr := telemetry.Open(conf.TelemetryExporter, telemetry.ExporterConfig{
			NodeID:      resolveUsageNodeID(conf.Fqdn),
			Environment: conf.Environment,
			Logger:      logger,
			HTTPClient:  &http.Client{Transport: tTransport, Timeout: 30 * time.Second},
			Secrets:     telemetrySecretSource(secretsResolver),
			Dropped:     dropped,
		})
		if terr != nil {
			cancel()
			return ctx, nil, terr
		}
		telemetryProc = telemetry.NewProcessor(texp, logger, dropped)
		logger.Info("telemetry exporter loaded",
			zap.String("exporter", texp.Name()))
	}

	// create communications channel
	bus := make(chan *event.Envelope)

	// Track in-flight bus-loop request goroutines so shutdown waits for
	// them to call tracer.End before we drain and close the trace sink.
	// Without this, async traces of mid-flight requests are lost on
	// SIGINT — the AsyncSink's Close runs before End is enqueued.
	var inflightWg sync.WaitGroup

	pu := processor.New(conf, logger, reg, mc, bus, kv, runtimeDB, authDB, dbc, runs, guard, secretsResolver)

	// Expose the trace sink on the processor so background goroutines
	// (local-async ExecMCPHTTP + the Resume they trigger) can spawn
	// resume traces — symmetric with what continuation.go does for
	// remote worker callbacks.
	pu.Sink = traceSink

	// Per-tenant admission gate (built above, rebuilt on dbcache reload).
	pu.Admission = admissionProv

	// In-process MCP session cache. Per (tenant, endpoint), 5min TTL.
	// Drops 3 HTTPS round-trips per MCP call to 1 on hot paths by
	// reusing the server-minted Mcp-Session-Id across calls. Per-
	// chassis (HA replicas don't share); swap to a shared backend in
	// v0.6 if monitoring shows the duplication cost is real.
	pu.EnableMCPSessionCache(5 * time.Minute)

	// Sandboxed-compute runtime. `op://name` rules resolve at apply time to
	// `compute://<alg>/<digest>`; at runtime the processor loads that
	// content-addressed wasm from the artifact store and runs it on the
	// registered engine (wazero, blank-imported above). Per-invocation limits
	// come from config. nil-safe everywhere — if this weren't set, a compute://
	// op would fail loudly rather than silently no-op.
	computeWall, werr := time.ParseDuration(conf.ComputeMaxWall)
	if werr != nil || computeWall <= 0 {
		computeWall = 250 * time.Millisecond
		if werr != nil {
			logger.Warn("invalid compute-max-wall; using default",
				zap.String("value", conf.ComputeMaxWall), zap.Duration("default", computeWall))
		}
	}
	pu.Computes = compute.NewManager(
		storeresolver.New(astore),
		compute.Limits{MaxMemoryMB: conf.ComputeMaxMemoryMB, MaxWall: computeWall},
	)
	// Per-compute usage events (src="compute") ride the same usage sink as
	// per-request usage. nil-safe when usage is disabled.
	pu.Usage = usageSink
	logger.Info("compute runtime loaded",
		zap.Int("max_memory_mb", conf.ComputeMaxMemoryMB),
		zap.Duration("max_wall", computeWall))

	// Register built-in core ops. `txco://noop` is the no-op handler:
	// rule authors use it as a placeholder EXEC when their rule's
	// purpose is to mutate the envelope via SET/SELECT rather than
	// dispatch to an external service. It just returns `{}`.
	//
	// `txco://echo` was removed — its only legitimate use was the
	// SET-PRE+EXEC-echo commit idiom, which EMIT now expresses more
	// precisely (literal-RHS overlay applied to op.Output, no leak of
	// the full input back into the merge). Any rule still using
	// `EXEC "txco://echo"` will surface the standard "op not found"
	// dispatch error.
	pu.Handle([]byte("txco://noop"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return event.Payload{Raw: "{}", Type: event.JSON}, nil
		}))

	// `txco://detect-tenant` is the hostname/listener/job → tenant
	// resolver, formerly hardcoded in dispatchEnvelope. It is the DECIDE
	// half of the editable _sys/boot pipeline (scope 0): it reuses the
	// exact same resolver (YAML-first, then the tenant_hostnames DB
	// lookup) and writes the result as an inert `_txc.route.*` proposal
	// — no jump, no re-tenant yet — so scopes before 100 can inspect or
	// override it for every request. On a miss it returns `{}` and
	// _sys/boot/1000 serves the 404.
	pu.Handle([]byte("txco://detect-tenant"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return event.Payload{Raw: detectTenantBody(resolver, in), Type: event.JSON}, nil
		}))

	// `txco://route` is the EXECUTE half (scope 100): pure mechanism
	// that promotes the `_txc.route.*` proposal into the active
	// `_txc.goto`/`_txc.tenant`(+parity) keys, triggering the one-way
	// _sys→tenant re-tenant gate + jump. The route-or-not decision is
	// the visible `WHEN @route.to != ""` gate on the boot/100 rule, not
	// in this op. Needed in Go only because txcl SET RHS is literal-only
	// (no path→path copy).
	pu.Handle([]byte("txco://route"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return event.Payload{Raw: routeBody(in), Type: event.JSON}, nil
		}))

	// `txco://continuation-result` is the poll/deferred-response handler
	// for the `?_txc.continuation=<rcid>` short-circuit (see
	// detectTenantBody / the internal _sys `txc-continuation` stack).
	// Like detect-tenant it's an explicit, trace-visible pipeline step:
	// the poll now shows in the trace list as a normal request instead
	// of the old bus-bypassing dedicated GET.
	pu.Handle([]byte("txco://continuation-result"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return event.Payload{Raw: continuationResultBody(ctx, runs, conf.ContinuationLongPollMS, in), Type: event.JSON}, nil
		}))

	// `txco://static` serves a layered (per-stack → chassis → embedded)
	// static file. boot/50 EXECs it unconditionally; the op self-gates
	// via an in-memory index, so a non-static request returns "{}" with
	// zero filesystem access. The index is built now and rebuilt on
	// every dbcache reload (stack activation, hostname change, fs-watch)
	// — never on the request path.
	staticIndex := static.NewIndex(conf.SystemOpstacksDir, logger)
	if dbc != nil {
		// Initial tenant FILES/ metadata build (path → content hash) from the
		// live snapshot; mirrors admission's Rebuild(dbc.Snapshot()).
		if rerr := staticIndex.RebuildTenant(dbc.Snapshot()); rerr != nil {
			logger.Warn("static tenant index initial build failed", zap.Error(rerr))
		}
		prevOnReload := dbc.OnReload
		dbc.OnReload = func(db *sql.DB) error {
			var err error
			if prevOnReload != nil {
				err = prevOnReload(db)
			}
			staticIndex.Rebuild()             // operator/disk + embedded layers
			_ = staticIndex.RebuildTenant(db) // tenant FILES/ metadata (hashes only)
			return err
		}
	}
	pu.Handle([]byte("txco://static"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return event.Payload{Raw: staticResultBody(ctx, staticIndex, fcas, in), Type: event.JSON}, nil
		}))

	// `txco://read-file` reads the routed stack's static FILES/ assets into
	// the envelope as DATA (vs `txco://static`, which serves a file as an
	// HTTP response). Same in-memory index + filecas, so it never touches
	// the filesystem on the request path; it CAN read `_`-private assets
	// (the FILES/_mail/ templating case). See chassis/server/readfile.go.
	pu.Handle([]byte("txco://read-file"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return readFile(ctx, staticIndex, fcas, in, conf.ReadFileMaxBytes)
		}))

	// Op-writable tenant KV (txco://kv/get|set|delete|incr|cas|list) — the only
	// ops that persist across requests. Backed by the configured KV store (boltdb or
	// redis, via --kvstore). Tenant-scoped via processor.TenantScope; namespace
	// defaults to the routed stack. See chassis/server/kv.go + chassis/kv.
	kvHandle := kvstore.New(kv, conf.KVMaxValueBytes, time.Duration(conf.KVMaxTTL)*time.Second)
	pu.Handle([]byte("txco://kv/get"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvGet(ctx, kvHandle, in)
		}))
	pu.Handle([]byte("txco://kv/set"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvSet(ctx, kvHandle, in)
		}))
	pu.Handle([]byte("txco://kv/delete"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvDelete(ctx, kvHandle, in)
		}))
	pu.Handle([]byte("txco://kv/incr"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvIncr(ctx, kvHandle, in)
		}))
	pu.Handle([]byte("txco://kv/cas"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvCAS(ctx, kvHandle, in)
		}))
	pu.Handle([]byte("txco://kv/list"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return kvList(ctx, kvHandle, in)
		}))

	// Durable tenant vector store (txco://vector/{collection,upsert,search,
	// delete}). The backend is selected by --vector-store (default "sqlite",
	// the bundled SQLite + sqlite-vec file). Tenant-scoped via
	// processor.TenantScope; collections are tenant-level (shared across
	// stacks), unlike the per-stack KV namespace. On open failure the ops stay
	// unregistered (report "op not found") rather than crash the chassis.
	vstore, verr := vector.Open(conf.VectorStore, vector.Config{DBPath: conf.VectorDBPath})
	// storeSeedMaterializers accumulates the store-seed materializers for the
	// stores that opened successfully; the Reconciler is built from them below
	// and injected into the admin controller so `txco apply` can declaratively
	// seed VECTORS/ (and, later, KV/) packs. See chassis/storeseed.
	var storeSeedMaterializers []storeseed.Materializer
	if verr != nil {
		pu.Logger.Warn("txco://vector disabled: " + verr.Error())
	} else {
		pu.Handle([]byte("txco://vector/collection"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return vectorCollection(ctx, vstore, in)
			}))
		pu.Handle([]byte("txco://vector/upsert"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return vectorUpsert(ctx, vstore, in)
			}))
		pu.Handle([]byte("txco://vector/search"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return vectorSearch(ctx, vstore, in)
			}))
		pu.Handle([]byte("txco://vector/delete"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return vectorDelete(ctx, vstore, in)
			}))
		storeSeedMaterializers = append(storeSeedMaterializers, vecseed.New(vstore, vstore.Shared()))
	}

	// txco://dataset — named-query lookups against the routed stack's
	// DATASETS/ SQLite artifacts (chassis/dataset). Tenant-scoped via
	// processor.TenantScope like kv/vector; per-stack like read-file.
	// Registered only when the dataset cache initialised (needs filecas);
	// otherwise the op reports "op not found" rather than half-working.
	if dsCache != nil {
		pu.Handle([]byte("txco://dataset"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return datasetQuery(ctx, pu.Dbc, dsCache, fcas, in, conf.DatasetMaxRows)
			}))
	}

	// KV store-seed materializer (KV/<namespace>.jsonl packs). kvHandle is
	// always constructed above, so this is unconditional. redis = fleet-shared
	// (reconcile once on the origin); boltdb (default) = per-node.
	kvShared := conf.KVStore == "redis"
	storeSeedMaterializers = append(storeSeedMaterializers, kvseed.New(kvHandle, kvShared))

	// Computed-secret core ops. These consume cleartext from
	// op.Secrets (plumbed onto ctx by processor.ExecCore) and emit
	// the **non-secret derived value** (HMAC digest / base64-encoded
	// credential) at output_path on the response envelope.
	// See internal docs/todo-secret-store.md §4.2 + chassis/ops/.
	pu.Handle([]byte("txco://hmac-sign"), event.OpsHandlerFunc(ops.HMACSign))
	pu.Handle([]byte("txco://hmac-verify"), event.OpsHandlerFunc(ops.HMACVerify))
	pu.Handle([]byte("txco://basic-auth-encode"), event.OpsHandlerFunc(ops.BasicAuthEncode))

	// Envelope-shape core ops. `txco://copy` is the path→path
	// primitive that closes the "txcl SET RHS is literal-only" gap
	// (same reason `txco://route` exists in Go). `txco://web-render`
	// composes that with the web-response shape — read a source path,
	// optionally render to HTML, set _txc.web.res.* + halt. Together
	// they cover the "scope N takes scope M's output and returns it
	// as the HTTP response" pattern without an external service.
	pu.Handle([]byte("txco://copy"), event.OpsHandlerFunc(ops.Copy))
	pu.Handle([]byte("txco://web-render"), event.OpsHandlerFunc(ops.WebRender))

	// `txco://sendmail`: render + submit an outbound email from the
	// `_sendmail` envelope contract a rule assembled. Struct-based op (needs
	// the real runtime DB for the campaign guard, the mirror snapshot for the
	// per-send From-domain/DKIM reads, the usage sink, and the relay config).
	// The closure passes the PINNED tenant (processor.TenantScope) — the
	// From-domain anti-spoof check must not trust the mutable `_txc.tenant`
	// envelope field.
	mailer := mail.NewMailer(pu.RuntimeDB, pu.RuntimeDialect, pu.Dbc.Snapshot, pu.Usage, logger, mail.Config{
		RelayAddr:     conf.MailRelayAddr,
		RelayTLS:      conf.MailRelayTLS,
		DialTimeout:   time.Duration(conf.MailDialTimeoutMS) * time.Millisecond,
		MaxRecipients: conf.MailMaxRecipients,
		RateLimits:    conf.MailRateLimits,
	})
	pu.Handle([]byte("txco://sendmail"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return mailer.Send(ctx, processor.TenantScope(ctx), in)
		}))

	// `txco://relay`: the `.forward` primitive — re-send an inbound message
	// VERBATIM (`_relay.raw`) to a new recipient, no re-compose/re-sign, so the
	// original DKIM survives. Shares the sendmail Mailer (same relay transport +
	// campaign guard + From-domain check). Passes BOTH the pinned tenant AND the
	// pinned SOURCE (processor.SourceScope): relay ships arbitrary bytes out
	// under a verified return-path, so it must run only from the inbound-mail
	// path — Relay refuses unless the source is "lmtp".
	pu.Handle([]byte("txco://relay"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return mailer.Relay(ctx, processor.TenantScope(ctx), processor.SourceScope(ctx), in)
		}))

	// `txco://schedule`: enqueue (or cancel/reschedule) a future event into the
	// scheduled_events store. Registered only when the scheduled personality
	// opened a store (--db-scheduled-dsn). The closure passes the PINNED tenant
	// (processor.TenantScope) — a scheduled event must fire for the tenant that
	// enqueued it, never a mutable `_txc.tenant`. See chassis/server/schedule.go.
	if scheduledStore != nil {
		pu.Handle([]byte("txco://schedule"), event.OpsHandlerFunc(
			func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
				return scheduleOp(ctx, scheduledStore, in)
			}))
	}

	// start controllers
	adminCtrl := admin.NewController(ctx, pu)
	// Wire the artifact store into the admin controller so its
	// mutating handlers can publish event payloads when fleet-sync
	// producer is enabled. Nil-safe; handlers gate on FeedSink != nop.
	adminCtrl.SetArtifactStore(astore)
	// Wire the filecas store so activation can persist FILES/ asset bytes
	// content-addressed. Nil-safe (gated on a configured store).
	adminCtrl.SetFileCAS(fcas)
	adminCtrl.SetDatasetCache(dsCache)
	// Wire the declarative store-seed reconciler so activation materialises
	// VECTORS/ (+ later KV/) packs into the runtime stores. Built from whatever
	// seedable stores opened above; nil-safe when none did.
	if len(storeSeedMaterializers) > 0 {
		adminCtrl.SetStoreReconciler(storeseed.NewReconciler(storeSeedMaterializers...))
	}
	// Wire the vector store for the inspect/teardown endpoints (`txco vector
	// ls/show/diff/rm`). Only when the store opened (verr == nil).
	if verr == nil {
		adminCtrl.SetVectorStore(vstore)
	}
	// Cross-node room fan-out (fleet). Empty --room-relay keeps rooms in-process
	// (single node). A relay-open failure degrades to in-process rather than
	// crashing — room chat is best-effort, unlike trace/control.
	if conf.RoomRelay != "" {
		if rerr := adminCtrl.EnableRoomRelay(conf.RoomRelay); rerr != nil {
			logger.Warn("room relay disabled — rooms stay single-node",
				zap.String("relay", conf.RoomRelay), zap.String("err", rerr.Error()))
		} else {
			logger.Info("room relay enabled", zap.String("relay", conf.RoomRelay))
		}
	}
	// LMTP head needs a MailResolver for per-recipient routing. The
	// data-plane resolver (`*DBResolver` wrapping the yamlResolver)
	// implements MailResolver directly. Assigning through the
	// interface lets a nil concrete resolver convert cleanly — the
	// LMTP head's per-rcpt loop nil-checks before calling, so an
	// embedder without ingress configured still gets a functional
	// head that default-denies every delivery.
	var mailResolver ingress.MailResolver = resolver

	webCtrl := web.NewController(ctx, pu, traceSink)
	dnsCtrl := dnsp.NewController(ctx, pu)

	// Bundled TLS: when --web-tls-addr is set the chassis terminates TLS
	// itself, obtaining + renewing wildcard certs for delegated zones via
	// in-process ACME DNS-01 against its OWN authoritative DNS head (the
	// solver writes the _acme-challenge into the dns head's challenge store,
	// which that head serves). Requires the 'dns' personality.
	var certMgr *txtls.Manager
	if strings.TrimSpace(conf.WebTLSAddr) != "" {
		if !strings.Contains(conf.Personalities, "dns") {
			logger.Warn("web-tls-addr set without the 'dns' personality: ACME DNS-01 has no authoritative server to answer challenges — enable 'dns' or terminate TLS at a front proxy")
		}
		m, mErr := txtls.NewManager(txtls.Options{
			Publisher:   dnsCtrl.ChallengeStore(),
			Email:       conf.ACMEEmail,
			CA:          conf.ACMECA,
			CARootFile:  conf.ACMECARootFile,
			StorageDSN:  conf.CertStorageDSN,
			StoragePath: conf.CertStoragePath,
			Resolvers:   conf.ACMEDNSResolvers,
			Logger:      logger,
		})
		if mErr != nil {
			cancel()
			return nil, nil, fmt.Errorf("bundled TLS: %w", mErr)
		}
		certMgr = m
		webCtrl.SetTLSConfig(certMgr.TLSConfig())
	}

	controllers := []controller{
		cronp.NewController(ctx, pu, cq),
		scheduledp.NewController(ctx, pu, scheduledStore),
		tcp.NewController(ctx, pu),
		webCtrl,
		adminCtrl,
		sweep.NewController(ctx, pu),
		lmtp.NewController(ctx, pu, mailResolver),
		// mailmap: a Postfix tcp_table responder answering the edge MTA's
		// relay_domains lookup against tenant_hostnames. Shares the SAME
		// mailResolver as the LMTP head so an accept decision can never
		// drift from a route decision. Off unless 'mailmap' is in
		// --personalities AND --mailmap-listen-addrs is set.
		mailmapp.NewController(ctx, pu, mailResolver),
		dnsCtrl,
		controlapply.NewController(ctx, pu, adminCtrl, fsrc, astore),
		controlpublish.NewController(ctx, pu, fsink),
	}

	// Background services: overlay-registered long-running loops the chassis
	// owns (started/stopped with the controllers above). Inert when none are
	// named (single-node default). They drive per-tenant admission state
	// through the admin controller's Gate; each reads its own backend config
	// (e.g. a shared-Postgres DSN) from its env in its constructor.
	bgsvcs, bgerr := bgservice.Open(conf.BackgroundServices, bgservice.Config{
		Logger: logger,
		Gate:   adminCtrl,
		NodeID: resolveUsageNodeID(conf.Fqdn),
	})
	if bgerr != nil {
		cancel()
		return ctx, nil, bgerr
	}
	for _, svc := range bgsvcs {
		controllers = append(controllers, svc)
	}

	// Start all controllers.
	for _, c := range controllers {
		c.Start()
	}

	// Self-serve the structured-host suffix's DNS: seed (idempotent) the
	// system-owned wildcard zone for --structured-host-suffix so the chassis is
	// authoritative for e.g. stacks.thanks.computer, and backfill per-host DKIM
	// keys for structured hosts minted before the 0017 columns. Runs AFTER
	// Start() — the admin controller's tenants store and the dns head's OnReload
	// are wired there; EnsureStructuredSuffixZone / Backfill reload the mirror,
	// so the dns snapshot rebuilds with the zone + per-host records. Control-
	// plane only (admin personality) + opt-in (--structured-dns-self); data-
	// plane nodes skip it and pick up the zone via fleet-sync.
	if conf.StructuredDNSSelf && strings.TrimSpace(conf.StructuredHostSuffix) != "" &&
		strings.Contains(conf.Personalities, "admin") {
		if err := adminCtrl.EnsureStructuredSuffixZone(ctx); err != nil {
			logger.Error("seed structured-suffix DNS zone failed", zap.Error(err))
		}
		if n, err := adminCtrl.BackfillStructuredHostDKIM(ctx); err != nil {
			logger.Error("backfill structured-host DKIM keys failed", zap.Error(err))
		} else if n > 0 {
			logger.Info("backfilled per-host DKIM keys", zap.Int("count", n))
		}
	}

	// Bundled TLS: with the dns head's initial zone snapshot now built,
	// obtain/renew the apex + wildcard cert per delegated zone, and recompute
	// that set whenever zones change — chained AFTER the dns head's own
	// snapshot rebuild on the same OnReload, so origins are already fresh.
	// manageCerts reads the atomic snapshot only (no Dbc lock), so it's safe
	// inside the OnReload hook (which runs while Dbc.Mu is held).
	if certMgr != nil {
		// certMu serializes cert passes so two reloads can't run overlapping
		// ACME obtains; Manage itself is idempotent (valid certs → fast).
		var certMu sync.Mutex
		manageCerts := func() {
			certMu.Lock()
			defer certMu.Unlock()
			domains := txtls.WildcardDomains(dnsCtrl.Origins())
			if err := certMgr.Manage(ctx, domains); err != nil {
				logger.Error("bundled TLS manage failed",
					zap.Strings("domains", domains), zap.String("err", err.Error()))
			}
		}
		go manageCerts() // initial obtain off the boot path
		if dbc != nil {
			prev := dbc.OnReload
			dbc.OnReload = func(db *sql.DB) error {
				var err error
				if prev != nil {
					err = prev(db)
				}
				// ACME is network-bound — a DNS-01 obtain for a freshly minted
				// host can take tens of seconds. The OnReload hook runs while
				// Dbc.Mu is held, so calling Manage synchronously here stalls
				// EVERY reader (healthz, ingress resolver, DNS) for the whole
				// obtain → that's the dbcache-reload 502 on stack activation.
				// Run it in the background instead; certMu serializes passes.
				go manageCerts()
				return err
			}
		}
	}

	go func() {

		for {
			stop := false
			select {
			case envelope := <-bus:
				if envelope != nil {
					eventRaw := envelope.Payload.Raw
					if logger.Core().Enabled(zap.DebugLevel) {
						logger.Debug("✉️", zap.String("raw", eventRaw))
					}

					// Drain: while this node is bleeding out of its load
					// balancer (SIGUSR1; SIGUSR2 to resume), reject new
					// requests with a 503 + Retry-After before dispatch.
					// In-flight requests already past this point finish
					// normally; /healthz also reports 503 so the LB stops
					// routing new traffic here.
					if admission.IsDraining() {
						emitDrainResponse(envelope)
						continue
					}

					// Every event enters the editable _sys/boot pipeline
					// (pinned to _sys); detect-tenant resolves the real
					// tenant and re-tenants into it, or _sys/boot 404s an
					// unrouted request. --ingress-miss-action=reject is the
					// hard opt-out: bare 404 without the boot pipeline.
					raw, stage := dispatchEnvelope(envelope.Payload.Raw, conf.IngressMissAction)
					envelope.Payload.Raw = raw

					if stage == stageNoRoute {
						// reject mode: bypass the processor and emit a
						// chassis-built 404 directly. The web inlet's
						// response writer reads the synthetic envelope
						// the same way it reads any stack-emitted
						// response. Log here (not in
						// emitNoRouteResponse) so operators see *which*
						// Host got rejected without firing up a trace.
						logger.Info("ingress reject (no_route)",
							zap.String("req_host", gjson.Get(raw, "_txc.web.req.host").String()),
							zap.String("rid", envelope.Rid),
							zap.String("src", gjson.Get(raw, "_txc.src").String()),
							zap.String("path", gjson.Get(raw, "_txc.web.req.url.path").String()))
						emitNoRouteResponse(envelope)
						continue
					}

					inflightWg.Add(1)
					go func() {
						defer inflightWg.Done()
						// Per-request admission lease: the concurrency gate
						// (processor) registers its slot-release on this lease;
						// the defer frees it when the request goroutine returns
						// (pipeline done OR continuation suspend). Nil-safe +
						// idempotent, so deferring unconditionally is fine.
						lease := admission.NewLease()
						defer lease.Release()
						reqCtx := admission.WithLease(envelope.Ctx, lease)
						// Usage attribution reads the tenant from immutable
						// pipeline state, not the mutable response envelope: an
						// author-controlled stack can rewrite `_txc.tenant`, so
						// billing trusts the pinned tenant the processor records
						// into this observer instead. See processor.TenantObserver.
						tenantObs := processor.NewTenantObserver()
						reqCtx = processor.WithTenantObserver(reqCtx, tenantObs)
						// kick off processor at the resolved entry stage.
						// runWithTrace wraps pu.Run with the per-request
						// tracer when tracing is enabled (mode != off); when
						// off, it's a direct call into pu.Run.
						reqStart := time.Now()
						finalPayload, fuelUsed, runErr := runWithTrace(reqCtx, pu, traceSink, envelope, raw, stage, usageSink != nil, telemetryProc != nil)
						if runErr != nil {
							logger.Warn("error adding event", zap.String("err", runErr.Error()))
						}

						// calculate response time
						resTime := int64(time.Since(reqStart) / time.Millisecond)

						// Usage event. This is the single convergence
						// point for every source; the processor emits
						// its response from several branches, but they
						// all funnel back here. The tenant is read from
						// the immutable observer the processor pinned —
						// NOT the response envelope, whose `_txc.tenant`
						// an author-controlled stack can rewrite. Fall
						// back to the ingress-stamped request (`_sys` for
						// unrouted) when nothing was pinned.
						if usageSink != nil {
							status := "ok"
							if runErr != nil {
								status = "error"
							}
							tenant, pinned := tenantObs.Tenant()
							if !pinned || tenant == "" {
								tenant = gjson.Get(raw, "_txc.tenant").String()
							}
							// Stack: the routed customer stack the request ran
							// (stamped onto the envelope by ingress routing,
							// next to _txc.tenant), so usage attributes to the
							// tenant's stack — not the _sys/boot entry stage. Fall
							// back to the entry stage for unrouted/_sys traffic
							// (no customer stack was stamped).
							stack := gjson.GetBytes(finalPayload, "_txc.stack").String()
							if stack == "" {
								stack = stage
							}
							// A request the admission gate denied (suspend / rate /
							// concurrency / drain) is non-usage: tag it so
							// billing/analytics never count rejected traffic as
							// load. The customer stack never ran, so zero fuel
							// regardless of any boot-stage accrual.
							denied := gjson.GetBytes(finalPayload, "_txc.admission.denied").Bool()
							fuel := fuelUsed
							if denied {
								fuel = 0
							}
							usageSink.WriteEvent(usage.UsageEvent{
								RID:             envelope.Rid,
								Tenant:          tenant,
								Src:             envelope.Src,
								Stack:           stack,
								DurationMS:      resTime,
								Status:          status,
								BytesIn:         len(eventRaw),
								BytesOut:        len(finalPayload),
								Fuel:            fuel,
								AdmissionDenied: denied,
								AdmissionReason: gjson.GetBytes(finalPayload, "_txc.admission.reason").String(),
								Billable:        !denied,
							})
						}
						// Tenant telemetry: hand _txc.telemetry.metrics
						// intents to the exporter. Same trust rule as
						// usage — the tenant comes from the immutable
						// observer — but STRICTER: no envelope fallback,
						// because export runs against the tenant's own
						// configured endpoint, so an unpinned request has
						// no identity to attribute that egress to (the
						// processor drops those, counted). Runs after the
						// response was written; context.WithoutCancel so
						// an already-canceled request ctx can't fail the
						// exporter's cold-start secret read.
						if telemetryProc != nil {
							tTenant, tPinned := tenantObs.Tenant()
							if !tPinned {
								tTenant = ""
							}
							tStack := gjson.GetBytes(finalPayload, "_txc.stack").String()
							if tStack == "" {
								tStack = stage
							}
							telemetryProc.Process(context.WithoutCancel(reqCtx), finalPayload, tTenant, tStack, envelope.Src)
						}
						if conf.LogOps != "" {
							go func() {
								err := logging.WriteOpsTop(&conf, envelope.Rid, envelope.Payload.Raw, nil)
								if err != nil {
									// TODO: error handling
									pu.Logger.Error("WriteOpsTop error", zap.String("err", err.Error()))
								}
							}()
						}

						// record event processed
						mc.RecordEvent(envelope.Ctx, envelope.Src, resTime)
					}()
				}
			case <-ctx.Done():
				logger.Info("Context done")
				stop = true
			}
			if stop {
				logger.Info("Server main loop stopping")
				break
			}
		}

	}()

	// Return a function that will stop all controllers.
	return ctx, func(reason string) {
		logger.Info("calling shutdown")
		cancel()

		var wg sync.WaitGroup
		for _, c := range controllers {
			wg.Add(1)
			go func(c controller) {
				defer wg.Done()
				c.Stop()
			}(c)
		}
		wg.Wait()
		close(bus)
		// Wait for in-flight request goroutines spawned from the bus
		// loop. Without this, their tracer.End calls race against
		// traceSink.Close and async traces get lost on shutdown. 5s
		// ceiling so a deadlocked request doesn't stall shutdown.
		inflightDone := make(chan struct{})
		go func() {
			inflightWg.Wait()
			close(inflightDone)
		}()
		select {
		case <-inflightDone:
		case <-time.After(5 * time.Second):
			logger.Warn("inflight wait timed out; trace records may be incomplete")
		}
		// Drain the trace sink (async wrapper waits for queued writes;
		// sync sinks are a no-op).
		traceFlushCtx, cancelTraceFlush := context.WithTimeout(context.Background(), 5*time.Second)
		if err := traceSink.Close(traceFlushCtx); err != nil {
			logger.Warn("trace sink shutdown error", zap.Error(err))
		}
		cancelTraceFlush()
		// Drain the usage sink. The default ZapSink is a no-op Close;
		// a future buffered/remote sink uses this to flush in-flight
		// events within the deadline.
		if usageSink != nil {
			usageFlushCtx, cancelUsageFlush := context.WithTimeout(context.Background(), 5*time.Second)
			if err := usageSink.Close(usageFlushCtx); err != nil {
				logger.Warn("usage sink shutdown error", zap.Error(err))
			}
			cancelUsageFlush()
		}
		// Drain tenant telemetry: flush every tenant's pending metric
		// aggregations. Runs after the inflight wait above, so every
		// bus-loop Process call has already handed its events over.
		if telemetryProc != nil {
			tFlushCtx, cancelTFlush := context.WithTimeout(context.Background(), 5*time.Second)
			if err := telemetryProc.Close(tFlushCtx); err != nil {
				logger.Warn("telemetry exporter shutdown error", zap.Error(err))
			}
			cancelTFlush()
		}
		// flush any pending OTel telemetry before exit
		flushCtx, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFlush()
		if err := mc.Shutdown(flushCtx); err != nil {
			logger.Warn("OTel shutdown error", zap.Error(err))
		}
		logger.Info("--thanks computer chassis shutdown--", zap.String("reason", reason))
	}, nil

}

// telemetrySecretSource adapts the chassis secret resolver to the
// telemetry seam: tenant-wide scope only (stack ""), with the store's
// not-found error mapped to the seam's sentinel so telemetry backends
// never import chassis/secrets. A nil resolver (some embedders/tests)
// reads as "no tenant has telemetry configured".
func telemetrySecretSource(r *secrets.Resolver) telemetry.SecretSource {
	return func(ctx context.Context, tenantSlug, name string) ([]byte, error) {
		if r == nil {
			return nil, telemetry.ErrSecretNotFound
		}
		cleartext, _, err := r.MaterializeForOpSlug(ctx, tenantSlug, "", name)
		if errors.Is(err, secrets.ErrSecretNotFound) {
			return nil, telemetry.ErrSecretNotFound
		}
		return cleartext, err
	}
}

// resolveUsageNodeID picks a stable identity for THIS chassis to stamp on
// usage rows, so a many-node store can attribute counters to a node.
// Prefers the operator-set FQDN; falls back to the OS hostname (distinct
// per container in a fleet); "local" as a last resort. Mirrors the cron
// head's resolveNodeID (unexported there).
func resolveUsageNodeID(fqdn string) string {
	if fqdn != "" {
		return fqdn
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}
