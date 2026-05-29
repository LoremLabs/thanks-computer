package processor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/abronan/valkeyrie/store"
	radix "github.com/hashicorp/go-immutable-radix"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/logging"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/registry"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/runtime"
	"github.com/loremlabs/thanks-computer/chassis/usage"
	"github.com/loremlabs/thanks-computer/chassis/utils/filematch"
)

// normalizeSelectPath converts a txcl-style envelope path into the
// dotted form gjson/sjson expect: `@foo` → `_txc.foo`, leading `.`
// stripped. Same shape as chassis/ops/copy.go's normalizePath; kept
// local to avoid a processor→ops import dependency.
func normalizeSelectPath(p string) string {
	if strings.HasPrefix(p, "@") {
		return "_txc." + strings.TrimPrefix(p, "@")
	}
	return strings.TrimPrefix(p, ".")
}

// CREATE TABLE ops (stack TEXT, scope INTEGER, txcl TEXT, mock_req TEXT, mock_res TEXT);

// Unit is the chassis's per-process processor handle. RuntimeDB holds
// content (ops, stacks, versions, files, tenants) and is always present.
// AuthDB holds identity (actors, keys, memberships, invitations, browser
// sessions) and is nil on data-plane-only chassis where the admin
// personality isn't active.
type Unit struct {
	Conf       config.Config
	Logger     *zap.Logger
	Kv         store.Store
	RuntimeDB  *sql.DB
	AuthDB     *sql.DB
	Dbc        *dbcache.DbCache
	Mc         *metrics.Metrics
	mu         sync.Mutex
	Bus        chan<- *event.Envelope
	Reg        registry.Registry
	Mux        *radix.Tree
	HTTPClient *http.Client

	// Runs is the continuation store wrapper. Used only on the barrier
	// (async) path; nil-safe — the sync fast path never touches it.
	Runs            *continuation.Runs
	CallbackBaseURL string

	// Sink spawns resume traces from background goroutines (local
	// async; future similar work). The same sink the web personality
	// uses, exposed here so chassis-internal goroutines that outlive
	// their originating request can write traces too. Nil-safe — when
	// unset, local async still functions but the resume work is
	// untraced (used in unit tests + non-server contexts).
	Sink trace.Sink

	// MCPSessions caches server-minted `Mcp-Session-Id` values per
	// (tenant, endpoint) so hot MCP paths skip the init lifecycle
	// (3 HTTPS round-trips → 1). Nil-safe — when unset, ExecMCPHTTP
	// runs the full lifecycle every call (test default; backwards-
	// compatible).
	MCPSessions *mcpSessionCache

	// Computes runs op:// computes resolved to "compute://<alg>/<digest>".
	// nil-safe: ExecCompute fails loudly if a compute:// op fires while
	// this is unset (no engine wired). Set when the chassis is built with
	// a compute runtime.
	Computes compute.Runner

	// Usage is the usage sink. nil-safe. When set, each compute invocation
	// emits a usage event (src="compute") alongside the per-request one.
	Usage usage.Sink

	// Secrets is the per-tenant secret-store Resolver. Non-nil when
	// --secret-master-key is configured AND its file loads cleanly at
	// boot; nil otherwise. PR 3 will consult this from the processor
	// splice that populates op.Secrets between WHEN/SET/SELECT/WITH
	// decoration and Exec. While Secrets is nil, any op with
	// `secrets` in its WITH clause must fail loud with
	// `secret_store_unavailable` rather than silently skip.
	Secrets *secrets.Resolver
}

// StagePartsRE Set compile limit for stage
var StagePartsRE = regexp.MustCompile(`(.*)\/+(\d+)$`)

// ctxKeyType is unexported so it can't collide with other packages'
// context keys. We use a single value of it for the opstack snapshot.
type ctxKeyType struct{ name string }

// ctxKeyOpstackSnap carries the *sql.DB pointer that a request was
// served with. See the comment on Run for the snapshot rationale.
var ctxKeyOpstackSnap = ctxKeyType{name: "opstack-snapshot"}

// ctxKeyTenant carries the tenant slug a request was routed to (from
// `_txc.tenant`, stamped by ingress). It is captured once on the first
// Run and pinned for the whole request — every recursive stage,
// including EXEC/goto stage jumps, resolves ops only within this
// tenant. Pinning at first Run (rather than re-reading `_txc.tenant`
// each stage) is the security property: a rule cannot mutate
// `_txc.tenant` mid-pipeline to escape into another tenant's stacks.
var ctxKeyTenant = ctxKeyType{name: "tenant-scope"}

// ctxKeyResumeRun carries the identity of an already-created run while a
// continuation is being resumed. When present, suspendBarrierScope reuses
// this run (a later async barrier on the SAME run = new stage docs, not a
// new run) and skips the client 202 (no client is waiting — it already
// got its 202 on the original request).
type resumeIdent struct{ runID, rcid string }

var ctxKeyResumeRun = ctxKeyType{name: "resume-run"}

func withResumeRun(ctx context.Context, runID, rcid string) context.Context {
	return context.WithValue(ctx, ctxKeyResumeRun, resumeIdent{runID: runID, rcid: rcid})
}

func resumeRunFrom(ctx context.Context) (resumeIdent, bool) {
	ri, ok := ctx.Value(ctxKeyResumeRun).(resumeIdent)
	return ri, ok && ri.runID != ""
}

// ctxKeyDeferredRun carries the run identity once a deferred-join op has been
// dispatched (internal docs/todo-deferred-join.md). It threads forward through the
// recursive Run calls so every later scope boundary knows to consult the
// run's pending joins (the floor check) and so a later same-scope barrier on
// the SAME run reuses this identity instead of minting a second run. Absent
// on the overwhelming common path (no deferred op dispatched) ⇒ the join
// check is skipped entirely, so there is zero overhead for ordinary runs.
//
// Carries rcid as well as runID because the deferred run is created up front
// (at dispatch, while the client is still attached): a later join suspend
// emits the FIRST client 202 and needs the run-continuation id for the poll
// URL — unlike the resume identity, which already handed the client its 202.
type deferredIdent struct{ runID, rcid string }

var ctxKeyDeferredRun = ctxKeyType{name: "deferred-run"}

func withDeferredRun(ctx context.Context, runID, rcid string) context.Context {
	return context.WithValue(ctx, ctxKeyDeferredRun, deferredIdent{runID: runID, rcid: rcid})
}

func deferredRunFrom(ctx context.Context) (deferredIdent, bool) {
	di, ok := ctx.Value(ctxKeyDeferredRun).(deferredIdent)
	return di, ok && di.runID != ""
}

// WithTenant pins a tenant slug onto ctx for op resolution. Exposed for
// callers that drive Run/OpsForStage outside the normal ingress path
// (and for tests). Within the normal data plane, Run pins this itself
// from the envelope's `_txc.tenant`.
func WithTenant(ctx context.Context, slug string) context.Context {
	return context.WithValue(ctx, ctxKeyTenant, slug)
}

// tenantScope returns the pinned tenant slug, or "" if the request is
// untenanted (boot/% fallback, system paths, tests). An empty scope
// means the op lookup is NOT tenant-filtered — preserving the legacy
// global behavior for un-routed requests, which never carry
// attacker-controlled tenant context.
func tenantScope(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTenant).(string); ok {
		return v
	}
	return ""
}

// tenantExists reports whether a non-revoked tenant with this slug
// exists in the opstack snapshot. Used to validate a boot re-tenant
// target before rebinding the pin.
func (pu *Unit) tenantExists(ctx context.Context, slug string) bool {
	var one int
	err := pu.opstackDB(ctx).QueryRowContext(ctx,
		`SELECT 1 FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug).Scan(&one)
	return err == nil
}

// maybeRetenant implements the one-way `_sys` -> concrete-tenant
// transition. It is the ONLY place a request's pinned tenant may
// change after the first Run.
//
// It fires only when the request is currently pinned to the system
// tenant — i.e. it arrived via the ingress-miss / boot path — and an
// operator-authored boot rule has rewritten `_txc.tenant` to a
// different, existing tenant (typically alongside an EXEC into that
// tenant's stack). Because firing rebinds the pin to a non-`_sys`
// tenant, and this gate only ever fires while pinned to `_sys`, it can
// never fire a second time for the same request: concrete->other and
// concrete->`_sys` are structurally impossible, not merely rejected.
// A real tenant's request never reaches here because it was pinned to
// that tenant at first Run and the guard below short-circuits.
func (pu *Unit) maybeRetenant(ctx context.Context, resp string) context.Context {
	if tenantScope(ctx) != tenants.SystemTenantSlug {
		return ctx // concrete tenant: pin is immutable.
	}
	target := gjson.Get(resp, "_txc.tenant").String()
	if target == "" || target == tenants.SystemTenantSlug {
		return ctx // no transition requested.
	}
	tr := trace.FromContext(ctx)
	if !pu.tenantExists(ctx, target) {
		pu.Logger.Warn("boot re-tenant rejected: unknown tenant",
			zap.String("target", target))
		tr.Event(trace.TimelineEvent{
			Ts: time.Now(), Event: "tenant.retenant_rejected",
			Fields: map[string]any{"target": target, "reason": "unknown_tenant"},
		})
		return ctx
	}
	pu.Logger.Debug("boot re-tenant",
		zap.String("from", tenants.SystemTenantSlug), zap.String("to", target))
	tr.Event(trace.TimelineEvent{
		Ts: time.Now(), Event: "tenant.retenant",
		Fields: map[string]any{"from": tenants.SystemTenantSlug, "to": target},
	})
	return WithTenant(ctx, target)
}

// opstackDB returns the per-request *sql.DB snapshot attached to ctx, or
// falls back to the chassis's current dbcache. The fallback handles
// callers (mostly tests) that drive OpsForStage without going through
// Run.
func (pu *Unit) opstackDB(ctx context.Context) *sql.DB {
	if v, ok := ctx.Value(ctxKeyOpstackSnap).(*sql.DB); ok && v != nil {
		return v
	}
	return pu.Dbc.Db
}

// New Processor. authDB may be nil on data-plane-only chassis. guard is
// the outbound op-dial policy consulted at the dial step (see egress).
func New(conf config.Config, logger *zap.Logger, reg registry.Registry, mc *metrics.Metrics, bus chan<- *event.Envelope, kv store.Store, runtimeDB, authDB *sql.DB, dbc *dbcache.DbCache, runs *continuation.Runs, guard egress.Guard, secretsResolver *secrets.Resolver) *Unit {

	mux := radix.New()

	// HTTP client used for op dispatch. Per-op timeouts are applied
	// via context.WithTimeout in Run() and are the *real* per-call
	// control (rules can bump them via `WITH timeout = "60s"`). The
	// client-level Timeout is a coarse safety-net ceiling — it must
	// be derived from OpTimeoutMax, NOT OpTimeout, otherwise the
	// client-side hard cap silently overrides per-op WITH timeouts
	// (e.g. an LLM-backed op like MCP ask_question taking 30s).
	clientTimeout, err := time.ParseDuration(conf.OpTimeoutMax)
	if err != nil {
		// Fall back to OpTimeout, then to a minute. The fallback
		// ladder keeps an old config without OpTimeoutMax working,
		// just less generously.
		clientTimeout, err = time.ParseDuration(conf.OpTimeout)
		if err != nil {
			clientTimeout = 60 * time.Second
		}
	}

	// Clone the stdlib default transport and bump the idle-connection
	// limits. Stock defaults cap idle keep-alive at 2 per host, which
	// chokes any chassis that fans out to a handful of ops under real
	// concurrency: each request opens fresh TCP sockets to the same op
	// service instead of reusing a warm connection. The values below
	// give plenty of headroom for a typical chassis fanout while still
	// bounding total in-flight connections per host.
	//
	// otelhttp.NewTransport then wraps the tuned transport so outbound
	// calls propagate trace context.
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.MaxIdleConns = 1000
	baseTransport.MaxIdleConnsPerHost = 1000
	baseTransport.MaxConnsPerHost = 0 // unlimited concurrent
	baseTransport.IdleConnTimeout = 10 * time.Second

	// Egress policy is enforced at the dial step. Control runs after DNS
	// resolution with the concrete IP about to be connected, so it
	// inspects the address actually dialed (DNS-rebinding safe). The
	// Timeout/KeepAlive mirror http.DefaultTransport's implicit dialer
	// so connection behaviour is otherwise unchanged. The default "open"
	// guard permits everything.
	baseTransport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   egress.DialControl(guard),
	}).DialContext

	httpClient := &http.Client{
		Transport: otelhttp.NewTransport(baseTransport),
		Timeout:   clientTimeout,
	}

	pu := &Unit{
		Conf:            conf,
		Logger:          logger,
		Kv:              kv,
		RuntimeDB:       runtimeDB,
		AuthDB:          authDB,
		Dbc:             dbc,
		Mc:              mc,
		Bus:             bus,
		Reg:             reg,
		Mux:             mux,
		HTTPClient:      httpClient,
		Runs:            runs,
		CallbackBaseURL: conf.ContinuationCallbackBaseURL,
		Secrets:         secretsResolver,
	}

	return pu
}

// Handle Setup the internal routing table, passing off opnames starting with prefix to this handler
func (pu *Unit) Handle(prefix []byte, handler event.OpsHandler) {
	pu.mu.Lock()
	defer pu.mu.Unlock()

	mux, _, _ := pu.Mux.Insert(prefix, handler)
	pu.Mux = mux
}

// Run Execute a request.
//
// Snapshot semantics: the first call to Run captures the current dbcache
// in-memory *sql.DB and attaches it to ctx. All subsequent recursive
// Run calls (for goto / next-stage advancement within the same request)
// reuse that snapshot. A `txco apply` that completes mid-request swaps
// pu.Dbc.Db on the chassis side but leaves the captured pointer
// untouched — Go's GC keeps the old in-memory DB alive while any
// request holds a reference. So the opstack a request sees at stage 0
// is the same opstack it sees at every later stage in the same request.
//
// Tests that drive Run directly without a snapshot in ctx still work:
// OpsForStage falls back to pu.Dbc.Db when the ctx key is absent.
func (pu *Unit) Run(ctx context.Context, raw string, stage string, resCh chan event.Payload) error {

	if ctx.Value(ctxKeyOpstackSnap) == nil {
		pu.Dbc.Mu.Lock()
		snap := pu.Dbc.Db
		pu.Dbc.Mu.Unlock()
		ctx = context.WithValue(ctx, ctxKeyOpstackSnap, snap)

		// Pin the tenant for the life of the request from the envelope
		// ingress stamped. Captured here (first Run only) so later
		// stages — including EXEC/goto jumps — stay boxed into this
		// tenant even if a rule rewrites `_txc.tenant`.
		if ctx.Value(ctxKeyTenant) == nil {
			ctx = context.WithValue(ctx, ctxKeyTenant, gjson.Get(raw, "_txc.tenant").String())
		}
	}

	pu.Logger.Debug("RUN!", zap.String("stage", stage), zap.String("in", raw))

	// Per-request secret cache. EnsureRequestCache installs only if
	// the outer Run hasn't already — inner Runs (stage jumps within
	// the same request) reuse the outer cache for dedup across
	// stages. The cleanup is a no-op when this is an inner Run.
	// See chassis/secrets/resolver.go.
	var secretsCacheCleanup func()
	ctx, secretsCacheCleanup = secrets.EnsureRequestCache(ctx)
	defer secretsCacheCleanup()

	// Emit a stage.enter event so the trace timeline reflects every
	// recursive Run call. Goto and natural advancement both end up
	// here.
	trace.FromContext(ctx).Event(trace.TimelineEvent{
		Ts:    time.Now(),
		Event: "stage.enter",
		Fields: map[string]any{
			"stage": stage,
		},
	})

	ctx, span := pu.Mc.Tracer.Start(ctx, `run-`+stage)
	defer span.End()

	// get all ops for stage
	ops, err := pu.OpsForStage(ctx, stage)
	if err != nil {
		pu.Logger.Debug("RUN err", zap.String("stage", stage), zap.String("err", err.Error()))
		return err
	}

	// Deferred-join floor check (internal docs/todo-deferred-join.md). Only when a
	// deferred op has been dispatched on this request (ctx carries the run);
	// the common path skips it entirely. Resolved scope = the floor scope
	// these ops actually live at (OpsForStage returns the MIN scope ≥ stage),
	// so the floor compares against where the run truly is, not the entry
	// stage. Merging happens BEFORE ResonatingOps so this scope's WHEN
	// filters see the merged deferred result.
	if di, ok := deferredRunFrom(ctx); ok {
		curStack, curScope := pu.stageScope(stage, ops)
		merged, stop, jerr := pu.resolveDeferredJoins(ctx, di, curStack, curScope, raw, resCh)
		if jerr != nil {
			return jerr
		}
		if stop {
			// 202 emitted (suspended at the join) or a deferred op failed —
			// either way this Run is done; the worker callback (or nothing)
			// drives it from here.
			span.End()
			return nil
		}
		raw = merged
	}

	// see which ones resonate
	ctx, span2 := pu.Mc.Tracer.Start(ctx, `resonatingops`)
	ops, err = pu.ResonatingOps(raw, ops, "")
	span2.End()
	if err != nil {
		pu.Logger.Warn("resonator error", zap.String("err", err.Error()))
		return err
	}

	// Deferred-join dispatch (internal docs/todo-deferred-join.md). Peel off async ops
	// whose join floor is a LATER scope: dispatch them now WITHOUT suspending
	// and continue. The remaining ops (sync + same-scope async) fall through
	// to the unchanged classification below. ctx gains the run identity so
	// later scope boundaries run the floor check above.
	if pu.Runs != nil {
		var derr error
		ctx, ops, derr = pu.dispatchDeferred(ctx, raw, stage, ops)
		if derr != nil {
			pu.Logger.Warn("deferred dispatch error", zap.String("err", derr.Error()))
			return derr
		}
	}

	// Get the opstack
	ctx, span4 := pu.Mc.Tracer.Start(ctx, `getopstack`)
	opstack, err := json.MarshalIndent(ops, " ", " ")
	span4.End()

	if err != nil {
		pu.Logger.Warn("opstack err 1", zap.String("err", err.Error()))
		return err
	}
	pu.Logger.Debug("opstack", zap.String("opstack", string(opstack)))

	// see if we have ops after this one (no goto here — just the natural scope+1)
	ctx, span5 := pu.Mc.Tracer.Start(ctx, `getnext`)
	ns := pu.nextStageFor(stage, "")
	nextOps, _ := pu.OpsForStage(ctx, ns)
	span5.End()
	// Pretty-printing the upcoming ops is debug-only trace; never pay
	// the MarshalIndent on the prod hot path, and a serialization
	// hiccup must not fail the request (it only feeds a log).
	if pu.Logger.Core().Enabled(zap.DebugLevel) {
		if nopstack, merr := json.MarshalIndent(nextOps, " ", " "); merr == nil {
			pu.Logger.Debug("next opstack", zap.String("stage", stage), zap.String("ns", ns), zap.String("nopstack", string(nopstack)))
		}
	}

	// Continuable classification. A scope containing any `WITH mode =
	// "continuable"` op races its upstream call against a timer; if the
	// timer wins it promotes to a continuation, otherwise it returns
	// inline. v1 requires the scope to be SOLO (one op) — mixed scopes
	// would need lazy "promote all in flight" semantics that aren't yet
	// needed. Checked BEFORE scopeHasAsync because a continuable op is
	// not classified as async (different mode value).
	if pu.Runs != nil && len(ops) > 0 {
		var continuableCount int
		for i := range ops {
			if isContinuableOp(ops[i]) {
				continuableCount++
			}
		}
		if continuableCount > 0 {
			if len(ops) != 1 {
				span.End()
				return pu.failContinuableInline(ctx, resCh,
					fmt.Errorf("mode=continuable requires a solo scope (v1); scope %s has %d ops",
						ops[0].Stack+"/"+strconv.Itoa(ops[0].Scope), len(ops)))
			}
			span.End()
			return pu.runScopeContinuable(ctx, raw, stage, "", ops, nextOps, resCh)
		}
	}

	// Barrier classification. A scope with ≥1 async op (WITH mode="async"
	// on an HTTP worker) cannot complete in-request: it suspends to the
	// continuation store and the request returns 202. A scope with zero
	// async ops falls straight through to the unchanged in-memory fast
	// path below — byte-identical to the pre-continuation behavior. The
	// store is only present when configured; nil ⇒ never barrier.
	if pu.Runs != nil && len(ops) > 0 && pu.scopeHasAsync(ops) {
		span6 := trace.FromContext(ctx)
		span6.Event(trace.TimelineEvent{
			Ts: time.Now(), Event: "stage.suspend",
			Fields: map[string]any{"stage": ops[0].Stack + "/" + strconv.Itoa(ops[0].Scope)},
		})
		span.End()
		return pu.suspendBarrierScope(ctx, raw, ops, resCh)
	}

	// run them
	var wg sync.WaitGroup
	responses := make(chan *operation.Operation)
	// errCh carries a fatal op error that must HALT the request (currently
	// compute:// ops — a thrown compute is a bug, not best-effort). Buffered
	// so the first failer never blocks; later failers drop (first error wins).
	// Other transports keep the legacy best-effort behavior (error swallowed).
	errCh := make(chan error, 1)
	var meta string

	ctx, span6 := pu.Mc.Tracer.Start(ctx, `run`)
	for _, op := range ops {
		wg.Add(1)

		// actual stage
		stage = op.Stack + "/" + strconv.Itoa(op.Scope)

		go func(op operation.Operation) {
			// op.Meta is the chassis-directives channel about this op (populated
			// from the rule's WITH clause). Read per-op timeout from there.
			// Number → milliseconds (matches "WITH timeout = 1000"). String →
			// time.ParseDuration ("500ms", "2s"). Anything else falls back to
			// the global OpTimeout default.
			timeout, _ := time.ParseDuration(pu.Conf.OpTimeout)
			if val := gjson.Get(op.Meta, "timeout"); val.Exists() {
				switch val.Type {
				case gjson.Number:
					timeout = time.Duration(val.Int()) * time.Millisecond
				case gjson.String:
					if parsed, err := time.ParseDuration(val.String()); err == nil {
						timeout = parsed
					}
				}
			}

			// Enforce the operator-set ceiling. A rule asking for more than
			// OpTimeoutMax is treated as a config error: we don't dispatch,
			// we log loudly, and we drop the op from the merge (the request
			// proceeds without its contribution, mirroring the existing
			// exec-error path). Config validation guarantees OpTimeoutMax
			// itself parses.
			if maxDur, err := time.ParseDuration(pu.Conf.OpTimeoutMax); err == nil && timeout > maxDur {
				pu.Logger.Error("op timeout exceeds op-timeout-max; rejecting op",
					zap.String("stack", op.Stack),
					zap.Int("scope", op.Scope),
					zap.String("name", op.Name),
					zap.Duration("requested", timeout),
					zap.Duration("max", maxDur))
				wg.Done()
				return
			}

			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			// Materialize secrets declared in WITH `secrets.*` before
			// Exec. Fast path: HasRefs is a single gjson hit; ops with
			// no `secrets` declaration skip entirely. Cleartext lives
			// in op.Secrets (per-op SecretBag), zeroed via defer on
			// every exit path from this goroutine — success, error,
			// timeout, panic. See internal docs/todo-secret-store.md §4.1.
			if secrets.HasRefs(op.Meta) {
				if pu.Secrets == nil {
					pu.Logger.Error("op declares `secrets` but the secret store is not configured (--secret-master-key unset)",
						zap.String("stack", op.Stack), zap.Int("scope", op.Scope), zap.String("op_name", op.Name))
					wg.Done()
					return
				}
				refs, perr := secrets.ParseRefs(op.Meta)
				if perr != nil {
					pu.Logger.Error("invalid `secrets` declaration",
						zap.String("stack", op.Stack), zap.Int("scope", op.Scope), zap.String("op_name", op.Name),
						zap.String("err", perr.Error()))
					wg.Done()
					return
				}
				tenantSlug := tenantScope(ctx)
				for _, name := range secrets.DistinctNames(refs) {
					cleartext, _, merr := pu.Secrets.MaterializeForOpSlug(ctx, tenantSlug, op.Stack, name)
					if merr != nil {
						pu.Logger.Error("secret materialization failed",
							zap.String("stack", op.Stack), zap.Int("scope", op.Scope),
							zap.String("op_name", op.Name), zap.String("secret_name", name),
							zap.String("err", merr.Error()))
						wg.Done()
						return
					}
					op.Secrets.Set(name, cleartext)
					// Per-reference counter (cache hits + misses).
					// Operationally useful for spotting a runaway op
					// that re-materializes the same secret thousands
					// of times per second, and for per-tenant audit/
					// quota signals. NEVER includes the cleartext.
					pu.Mc.RecordSecretMaterialize(ctx, tenantSlug, name)
				}
				// Wipe cleartext on every exit path. Cache cleanup
				// (installed at Run head) zeros the cache's own copies;
				// this wipes the bag's view.
				defer op.Secrets.Zero()
			}

			// exec function
			if op.Resonator != nil {
				var output event.Payload
				var err error
				// A rule with no EXEC clause is semantically identical
				// to `EXEC "txco://noop"` — both produce `{}` and let
				// EMIT (if any) overlay onto it. Routing both through
				// pu.Exec also keeps trace.Step recording and the
				// _txc.mocks pattern-match interception symmetric:
				// pattern-mocking an op shouldn't require the rule
				// author to add a dummy EXEC.
				output, err = pu.Exec(ctx, op)
				if err != nil {
					pu.Logger.Debug("outerr", zap.String("err", err.Error()))
				} else {
					pu.Logger.Debug("out", zap.String("out", output.String()))
				}

				// Outbound op control flow (halt, goto, ...) lives in `_txc.*`
				// in the response body, read from the merged envelope after this
				// stage completes. Op.Meta is no longer overwritten here — it
				// stays as the inbound chassis-directives channel populated by
				// WITH. The legacy `meta` accumulator is kept only for the
				// `..#.delete` reader below; the modern form is `_txc.delete`
				// on the merged envelope (authored as `EMIT @delete = [...]`),
				// applied alongside it in advanceAfterScope.
				if len(output.Meta) > 0 {
					meta = meta + output.Meta + "\n"
				}

				if err == nil {
					if output.Type == event.JSON {
						op.Output = output.Raw
					}
					if output.Type == event.Null {
						op.Output = `{}`
					}

					// EMIT overlays values onto THIS op's response
					// before it reaches the per-scope merge. Overwrite
					// semantics (not the set-if-absent behavior of
					// SET POST on the merged response). Lets a rule
					// contribute literal fields without needing a real
					// handler — pair with EXEC for "enrich the
					// response", or write EMIT alone for a synthetic
					// emitter (no EXEC needed; we synthesized "{}"
					// above).
					if op.Resonator.Emit != nil {
						out, oerr := pu.OverlayResponse(op.Input, op.Output, op.Resonator.Emit.Overrides)
						if oerr != nil {
							pu.Logger.Debug("emit overlay", zap.String("err", oerr.Error()))
							op.Output = string(failPayload(oerr.Error()))
						} else {
							op.Output = out
						}
					}

					responses <- &op
				} else if strings.HasPrefix(op.Resonator.Exec, "compute://") {
					// A compute throwing is a bug, not best-effort: be loud and
					// halt the request (the main loop drains errCh and emits an
					// error response). First error wins; the buffered send never
					// blocks.
					pu.Logger.Error("compute failed",
						zap.String("stack", op.Stack), zap.Int("scope", op.Scope),
						zap.String("op_name", op.Name), zap.String("err", err.Error()))
					select {
					case errCh <- err:
					default:
					}
					wg.Done()
				} else {
					// Other transports stay best-effort: drop this op's output
					// and continue (legacy behavior).
					pu.Logger.Debug("exec-err", zap.String("err", err.Error()))
					wg.Done()
				}
			}
		}(op)
	}
	span6.End()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// merge
	resp := raw
	opsDone := false
	// failRun halts the request: emit an error response and return the cause.
	failRun := func(e error) error {
		resCh <- event.Payload{Raw: string(failPayload(e.Error())), Type: event.ErrorStr}
		span.End()
		return e
	}
	for {
		select {
		case <-done:
			// A halting op error (compute) wins over emitting a final
			// response — check it before advancing, so a failure that lands
			// together with `done` still surfaces as an error.
			select {
			case e := <-errCh:
				return failRun(e)
			default:
			}
			stop, derr := pu.advanceAfterScope(ctx, stage, resp, ops, meta, nextOps, &opsDone, resCh, func() { span.End() })
			if stop {
				return derr
			}
		case e := <-errCh:
			return failRun(e)
			// Not stopped: the only non-stop path is the default branch's
			// first pass (final response emitted, opsDone now true). The
			// closed `done` channel re-selects; the next advanceAfterScope
			// hits the opsDone branch and stops. Behavior is identical to
			// the pre-refactor inline loop.
		case op := <-responses:
			pu.Logger.Debug("res", zap.String("response", op.Output))
			if strings.Compare(op.Output, "{}") != 0 {
				_, span7 := pu.Mc.Tracer.Start(ctx, `merge`)
				resp, err = pu.MergeJSON(resp, op.Output)
				span7.End()

				if err != nil {
					pu.Logger.Warn("merge-err", zap.String("err", err.Error()))
				}
			}
			// fmt.Println("merged\n" + resp + "\n")
			wg.Done()
		case <-ctx.Done():
			response := event.Payload{
				Raw:  `{"err":"canceled"}`,
				Type: event.ErrorStr,
			}
			resCh <- response
			return errors.New("context canceled")
		}
	}
}

// advanceAfterScope runs the post-merge decision for one scope: SET POST
// decoration, legacy ..#.delete, _txc.halt/_txc.goto resolution,
// breakpoint handling, then the terminal switch (halt/break → emit+stop;
// goto/next → recurse pu.Run; default → emit final). It is the single
// point that decides what happens once a scope's merged envelope (`resp`)
// is known.
//
// Extracted verbatim from Run's `case <-done:` so the synchronous path is
// behavior-identical (existing processor tests are the safety net). It is
// also the resume entry point: the continuation callback rebuilds the
// ordinal-merged envelope and calls this with endSpan = no-op.
//
// Returns stop=true when Run should return (with err); stop=false only on
// the default branch's first pass (final emitted, *opsDone set true) so
// the caller's loop re-selects the closed `done` channel and the next
// call stops via the opsDone branch — exactly the pre-refactor flow.
func (pu *Unit) advanceAfterScope(
	ctx context.Context,
	stage string,
	resp string,
	ops []operation.Operation,
	meta string,
	nextOps []operation.Operation,
	opsDone *bool,
	resCh chan event.Payload,
	endSpan func(),
) (stop bool, err error) {

	pu.Logger.Debug("done", zap.String("resp", string(resp)))

	tr := trace.FromContext(ctx)
	tr.Event(trace.TimelineEvent{
		Ts:    time.Now(),
		Event: "stage.merge",
		Fields: map[string]any{
			"stage":      stage,
			"resp_bytes": len(resp),
		},
	})

	// The `nextOps` passed in was computed against the entry-stage
	// scope (Run's caller didn't know which scope the floor-lookup
	// would auto-advance to). When the entry was sparse — e.g. a
	// goto landed at "stack/0" but the rules live at scope 100 —
	// nextOps was computed at scope > entry+1, which floor-looks
	// up the SAME scope we just executed, so the advance decision
	// below would recurse into the same scope and re-fire the
	// rule. Re-query using the actual executed scope as the floor
	// (`ranScope + 1`) so the next-stage probe sees only ops
	// strictly above what we just ran. No-op when nothing ran
	// (empty `ops`) — the original nextOps stands.
	if len(ops) > 0 {
		ranScope := 0
		for _, op := range ops {
			if op.Scope > ranScope {
				ranScope = op.Scope
			}
		}
		if curStack, _, perr := pu.StageParse(stage); perr == nil {
			corrected := curStack + "/" + strconv.Itoa(ranScope+1)
			if reOps, perr := pu.OpsForStage(ctx, corrected); perr == nil {
				nextOps = reOps
			}
		}
	}

	// Evaluate State of World. Should we continue?
	// decorate output with any POST commands from our opstack (this ensures structures exist)
	for _, op := range ops {
		if op.Resonator.SetPost != nil {
			out, derr := pu.DecorateInput(resp, op.Resonator.SetPost.Overrides) // only if it doesn't exist
			if derr != nil {
				// SET POST resolution failed for this op. Per strict-by-default
				// semantics, the merged response is replaced with a failure
				// payload so downstream consumers see the error rather than a
				// silently-incomplete merge. PR 2 can't trigger this path
				// (literals never error); PR 3 onward must consider whether
				// per-op halt is the right granularity here.
				pu.Logger.Debug("decorate post", zap.String("op", op.Name), zap.String("err", derr.Error()))
				resp = string(failPayload(derr.Error()))
				break
			}
			resp = out
		}
	}

	// remove anything we should delete (legacy ..#.delete in op meta)
	val := gjson.Get(meta, "..#.delete")
	if len(val.Array()) > 0 {
		for _, del := range val.Array() {
			for _, b := range del.Array() {
				branch := b.String()
				branch = strings.TrimPrefix(branch, ".")
				resp, _ = sjson.Delete(resp, branch)
			}
		}
	}

	// `_txc.delete`: envelope paths to remove, authored as
	// `EMIT @delete = ["london", "tokyo"]` (or a single string). Applied
	// here, post-scope — so a later-scope op can read a value before an
	// even-later op (or the same op) deletes it (e.g. summary@100 reads
	// .london.time, then trim@200 deletes .london). It mutates the merged
	// envelope itself, so the deletion is visible to every transport AND
	// the trace's final `out`, not just the web projection. The directive
	// is stripped after applying so it neither leaks into the response nor
	// re-fires on the next scope. Supersedes the legacy `..#.delete` meta
	// channel above; both are honored. Accepts dotted paths (e.g.
	// "london.year") and `@`-prefixed `_txc.` paths via normalizeEnvelopePath.
	if dels := gjson.Get(resp, "_txc.delete"); dels.Exists() {
		for _, d := range dels.Array() {
			if p := normalizeEnvelopePath(d.String()); p != "" {
				resp, _ = sjson.Delete(resp, p)
			}
		}
		resp, _ = sjson.Delete(resp, "_txc.delete")
	}

	// Op-set control flow lives in `_txc.*` in the merged response.
	// Read halt/goto, then strip them so they don't accumulate across
	// stages or leak into the user-facing envelope.
	halt := gjson.Get(resp, "_txc.halt").Bool()
	gotoStage := pu.resolveGoto(stage, gjson.Get(resp, "_txc.goto").String())
	if halt || gotoStage != "" {
		resp, _ = sjson.Delete(resp, "_txc.halt")
		resp, _ = sjson.Delete(resp, "_txc.goto")
	}

	// Breakpoint: only honored when _txc.flag_breakpoint is set on
	// the envelope (the chassis-level opt-in stamped by the inlet
	// when --debug-breakpoints is enabled). The break value names
	// a scope threshold; we halt AFTER the current scope's merge
	// in either case:
	//   * cur >= N — we matched (or a rule SET break to overshoot)
	//   * cur < N AND the next stage would land at scope > N —
	//     about to cross the threshold; halt here so we don't
	//     fire past it.
	// Sparse scope numbering (100, 200, 1000) therefore works
	// without the developer knowing the exact value: break=999
	// lands at scope 200 (latest scope ≤ 999) rather than running
	// through to render. break=2000 with no scope above never
	// halts — pipeline ends naturally. Setting _txc.break from a
	// rule without the flag does nothing.
	breakHere := false
	breakAt := 0
	if gjson.Get(resp, "_txc.flag_breakpoint").Bool() {
		if br := gjson.Get(resp, "_txc.break"); br.Exists() {
			N := br.Int()
			if curStack, curScope, perr := pu.StageParse(stage); perr == nil {
				// Optional stack gate: when `_txc.break_stack` is set
				// (`?_txc.break=stack/N` form), only break when we're
				// in that stack. The value stays on the envelope
				// across scopes that don't match so it can fire
				// once we land in the target stack.
				wantStack := gjson.Get(resp, "_txc.break_stack").String()
				stackMatches := wantStack == "" || wantStack == curStack
				var nextScope int64 = -1
				hasNext := false
				if gotoStage != "" {
					if _, gs, gerr := pu.StageParse(gotoStage); gerr == nil {
						nextScope = int64(gs)
						hasNext = true
					}
				} else {
					// `nextOps` was corrected at the top of
					// advanceAfterScope to reflect the actual scope
					// we executed (not the entry stage), so it's
					// safe to consult directly here. The previous
					// in-place re-query is now redundant.
					if len(nextOps) > 0 {
						nextScope = int64(nextOps[0].Scope)
						hasNext = true
					}
				}
				switch {
				case stackMatches && int64(curScope) >= N:
					breakHere = true
					breakAt = curScope
				case stackMatches && hasNext && nextScope > N:
					breakHere = true
					breakAt = curScope
				}
			}
		}
	}
	if breakHere {
		resp, _ = sjson.Delete(resp, "_txc.break")           // consumed
		resp, _ = sjson.Delete(resp, "_txc.break_stack")     // consumed
		resp, _ = sjson.Delete(resp, "_txc.flag_breakpoint") // internal, don't leak
		resp, _ = sjson.Set(resp, "_txc.broke_at", breakAt)
		// Enrich the response with the opstack shape — the (step,
		// ops) list for the current stack. Helps devs see what
		// rules are wired up without going to the admin API.
		if curStack, _, perr := pu.StageParse(stage); perr == nil {
			if opstackJSON, oerr := pu.buildOpstack(ctx, curStack); oerr == nil {
				resp, _ = sjson.SetRaw(resp, "_txc.opstack", opstackJSON)
			}
		}
	}

	// HTTP response streaming (explicit-body only). A `_txc.web.res.body`
	// written in a NON-terminal scope is treated as a chunk: flush it now,
	// clear it, and let the pipeline continue producing. The first flush
	// emits a StreamHead (status + headers snapshot) and locks the head —
	// later status/header edits no longer reach the client, exactly as
	// HTTP requires once the first body byte is written. A body written in
	// the terminal scope (the dominant pattern: body + `@halt` together) is
	// NOT streamed; it falls through to the normal buffered emit below,
	// which preserves Content-Length. Breakpoints pre-empt streaming
	// entirely so `_txc.broke_at` keeps dumping the whole envelope.
	streamingAllowed := !gjson.Get(resp, "_txc.flag_breakpoint").Bool()
	streamOpen := gjson.Get(resp, "_txc.runtime.http_stream_open").Bool()
	isContinuing := !(halt || breakHere) && ((len(gotoStage) > 0) || (len(nextOps) > 0))
	if streamingAllowed && (streamOpen || isContinuing) {
		body := gjson.Get(resp, "_txc.web.res.body").String()
		if isContinuing {
			if body != "" {
				if !streamOpen {
					resCh <- event.Payload{Raw: resp, Type: event.StreamHead}
				}
				if dec, derr := base64.StdEncoding.DecodeString(body); derr == nil {
					resCh <- event.Payload{Raw: string(dec), Type: event.StreamChunk}
				} else {
					pu.Logger.Warn("stream chunk decode", zap.String("err", derr.Error()))
				}
				resp, _ = sjson.Delete(resp, "_txc.web.res.body")
				resp, _ = sjson.Set(resp, "_txc.runtime.http_stream_open", true)
			}
			// Not terminal: fall through to the goto/next branch, which
			// recurses pu.Run with the (body-stripped) resp.
		} else if streamOpen {
			// Terminal scope while a stream is open: emit the final body (if
			// any) as the last chunk, then close the stream. Do NOT fall
			// through to the normal JSON emit. (breakHere can't reach here —
			// it requires flag_breakpoint, which disables streaming above.)
			if !*opsDone {
				if body != "" {
					if dec, derr := base64.StdEncoding.DecodeString(body); derr == nil {
						resCh <- event.Payload{Raw: string(dec), Type: event.StreamChunk}
					} else {
						pu.Logger.Warn("stream chunk decode", zap.String("err", derr.Error()))
					}
				}
				resCh <- event.Payload{Type: event.StreamEnd}
				*opsDone = true
				// Deferred-join in-request finalize (mirrors the halt branch).
				if di, ok := deferredRunFrom(ctx); ok {
					if _, resuming := resumeRunFrom(ctx); !resuming {
						if werr := pu.Runs.WriteResult(ctx, di.runID, []byte(resp)); werr == nil {
							_ = pu.Runs.AppendEvent(ctx, di.runID, "run.completed",
								map[string]any{"stage": stage, "in_request": true, "stream": true})
						}
					}
				}
			}
			endSpan()
			return true, nil
		}
	}

	switch {
	case halt || breakHere:
		// halt or break: emit the merged response and return.
		// Both follow the same emit-and-stop pattern; the log
		// line distinguishes which one fired for diagnostics.
		if breakHere {
			pu.Logger.Debug("breakpoint", zap.String("stage", stage), zap.Int("scope", breakAt))
			tr.Event(trace.TimelineEvent{
				Ts: time.Now(), Event: "request.break",
				Fields: map[string]any{"stage": stage, "broke_at": breakAt},
			})
		} else {
			pu.Logger.Debug("halt requested by op", zap.String("stage", stage))
			tr.Event(trace.TimelineEvent{
				Ts: time.Now(), Event: "request.halt",
				Fields: map[string]any{"stage": stage},
			})
		}
		if !*opsDone {
			resCh <- event.Payload{Raw: string(resp), Type: event.JSON}
			// Deferred-join in-request finalize (see the default branch).
			if di, ok := deferredRunFrom(ctx); ok {
				if _, resuming := resumeRunFrom(ctx); !resuming {
					if werr := pu.Runs.WriteResult(ctx, di.runID, []byte(resp)); werr == nil {
						_ = pu.Runs.AppendEvent(ctx, di.runID, "run.completed",
							map[string]any{"stage": stage, "in_request": true, "halt": true})
					}
				}
			}
		}
		endSpan()
		return true, nil
	case (len(gotoStage) > 0) || (len(nextOps) > 0):
		// Natural advancement jumps straight to the next non-empty
		// scope rather than walking scope+1, scope+2, … through gaps.
		// OpsForStage's floor lookup at scope+1 already returned the
		// MIN-scope ops at-or-above, so nextOps[0].Scope is provably
		// the next scope with any rules at all. The intermediates
		// have nothing to evaluate — walking them does the same SQL
		// + ResonatingOps + tracer-span work for an empty rule set
		// every time. For sparse scope layouts (the 0/100/1000
		// convention) that's hundreds of redundant recursive Run
		// calls per request, and the OTel span tree grows linearly
		// with the gap. The breakpoint logic above already used this
		// re-query trick locally; we apply the same insight to the
		// natural-advancement path.
		//
		// The current request's stack — not nextOps[0].Stack — is
		// preserved across the jump. OpsForStage walks stack-prefix
		// ancestry on a miss (a request at website/canary/N can
		// inherit ops from website/N), and the request must stay
		// boxed into its own stack namespace even when running rules
		// it inherited from a parent.
		var nextStage string
		if gotoStage != "" {
			nextStage = gotoStage
			pu.Logger.Debug("going to", zap.String("gotostage", gotoStage))
		} else {
			curStack, _, perr := pu.StageParse(stage)
			if perr != nil {
				// Defensive: shouldn't happen — we just ran Run on
				// this stage, so it parsed once already. Fall back
				// to the old scope+1 behavior rather than dropping
				// the request.
				nextStage = pu.nextStageFor(stage, "")
			} else {
				nextStage = curStack + "/" + strconv.Itoa(nextOps[0].Scope)
				pu.Logger.Debug("NEXT", zap.String("stage", nextStage))
			}
		}
		if pu.Logger.Core().Enabled(zap.DebugLevel) {
			pu.Logger.Debug("res", zap.String("response", string(resp)))
		}
		if gotoStage != "" {
			tr.Event(trace.TimelineEvent{
				Ts: time.Now(), Event: "stage.jump",
				Fields: map[string]any{"from": stage, "to": gotoStage},
			})
		}
		// One-way _sys -> concrete-tenant handoff. No-op unless
		// this request is in the boot/_sys context and a boot
		// rule re-tenanted it; from a concrete tenant the pin is
		// immutable. The boot rule is responsible for also
		// EXEC-ing into the target tenant's stack — the pin
		// rebind only changes which tenant's ops resolve.
		runCtx := pu.maybeRetenant(ctx, string(resp))
		endSpan()
		return true, pu.Run(runCtx, string(resp), nextStage, resCh)
	default:
		if !*opsDone {
			response := event.Payload{
				Raw:  string(resp),
				Type: event.JSON,
			}

			*opsDone = true // any future responses from this opstack will be ignored
			resCh <- response

			// Deferred-join in-request completion: a deferred op was
			// dispatched on this request and the pipeline ran to a terminal
			// without ever suspending (every join merged in-request). Persist
			// the run's terminal so the sweeper doesn't later flag the
			// already-returned run as "expired". Skipped under resume — there
			// resumeDeferredJoin owns finalization via its capture channel.
			// WriteResult is create-if-absent, so this is a no-op if a result
			// already exists. See internal docs/todo-deferred-join.md.
			if di, ok := deferredRunFrom(ctx); ok {
				if _, resuming := resumeRunFrom(ctx); !resuming {
					if werr := pu.Runs.WriteResult(ctx, di.runID, []byte(resp)); werr != nil {
						pu.Logger.Warn("deferred in-request finalize failed",
							zap.String("run", di.runID), zap.Error(werr))
					} else {
						_ = pu.Runs.AppendEvent(ctx, di.runID, "run.completed",
							map[string]any{"stage": stage, "in_request": true})
					}
				}
			}

			rid, ok := ctx.Value(config.CtxKeyRid).(string)
			if (!ok) || (rid == "") {
				rid = "unset" // TODO: an error?
			}
			if pu.Conf.LogOps != "" {
				go func() {
					err := logging.WriteOpsTop(&pu.Conf, rid, "", &response)
					if err != nil {
						pu.Logger.Warn("WriteOpsTop error", zap.String("err", err.Error()))
					}
				}()
			}
		} else {
			// Expected side-effect of a request that already finalized
			// (typically: web-inlet response timeout, ctx canceled, the
			// resCh consumer is gone). The op's work product can't be
			// delivered, so we drop it. Not a real warning — operators
			// who want to see this for diagnosis can turn on debug.
			pu.Logger.Debug("OpsDone, Ignoring response")
			return true, nil
		}
	}
	return false, nil
}

// isAsyncOp reports whether op is an async op: its WITH clause set
// mode="async" AND the EXEC scheme is one the chassis knows how to
// run asynchronously. Two flavors share this gate today:
//   - Remote async: http(s):// EXEC. Chassis POSTs a job envelope
//     to the worker and suspends; worker calls back with the result.
//   - Local async: mcp+http(s):// EXEC. Chassis runs ExecMCPHTTP
//     itself in a background goroutine and writes the terminal
//     directly — no worker, no callback URL needed. isLocalAsyncOp
//     distinguishes the two so the dispatch path can branch.
func isAsyncOp(op operation.Operation) bool {
	if op.Resonator == nil {
		return false
	}
	if gjson.Get(op.Meta, "mode").String() != "async" {
		return false
	}
	ex := op.Resonator.Exec
	return strings.HasPrefix(ex, "http://") ||
		strings.HasPrefix(ex, "https://") ||
		strings.HasPrefix(ex, "mcp+http://") ||
		strings.HasPrefix(ex, "mcp+https://")
}

// isLocalAsyncOp reports whether op runs in a chassis-internal
// goroutine (rather than via the remote-worker callback contract).
// Distinct from isAsyncOp: the latter is the umbrella gate; this is
// the per-op routing choice. `mode = "async"` on plain HTTP(S) uses
// the worker-callback contract; `mcp+http(s)://` skips that contract
// because MCP servers aren't expected to know about it. Upstreams that
// "take their time and return when done" without speaking the contract
// belong on `mode = "continuable"`, not here.
func isLocalAsyncOp(op operation.Operation) bool {
	if !isAsyncOp(op) {
		return false
	}
	ex := op.Resonator.Exec
	return strings.HasPrefix(ex, "mcp+http://") ||
		strings.HasPrefix(ex, "mcp+https://")
}

// isContinuableOp reports whether op is `WITH mode = "continuable"` on an
// HTTP(S) or MCP+HTTP(S) URL. Separate from isAsyncOp because the dispatch
// timing differs: continuable starts SYNC and only promotes to a
// continuation if the upstream takes longer than continue_after. The
// chassis bridges the worker contract locally for upstreams that don't
// know about it — `EXEC "https://slow.example/api" WITH mode = "continuable"`
// works against any plain HTTP service.
func isContinuableOp(op operation.Operation) bool {
	if op.Resonator == nil {
		return false
	}
	if gjson.Get(op.Meta, "mode").String() != "continuable" {
		return false
	}
	ex := op.Resonator.Exec
	return strings.HasPrefix(ex, "http://") ||
		strings.HasPrefix(ex, "https://") ||
		strings.HasPrefix(ex, "mcp+http://") ||
		strings.HasPrefix(ex, "mcp+https://")
}

// dispatchLocalAsync runs a local-async op fire-and-forget:
//
//   - Records a 'pending' step on the suspending request's trace
//     (synchronous; same shape as remote async's dispatch ack)
//     so the original trace shows the barrier op even though the
//     work hasn't finished.
//   - Spawns a detached goroutine with a fresh background context
//     so the request's ctx ending after the 202 emit doesn't kill
//     the in-flight op. The per-op timeout (WITH timeout = "60s")
//     is still honored via WithTimeout on the fresh ctx.
//   - On completion the goroutine writes the terminal and, if the
//     stage is now fully resumable, claims and Resume's it —
//     symmetric with the remote-worker callback handler's flow.
func (pu *Unit) dispatchLocalAsync(reqCtx context.Context, op operation.Operation, runID, stage string, ordinal int, name string) {
	// Honor op-timeout-max even though the work runs detached.
	timeout, over := pu.opMetaTimeout(op)
	if over {
		pu.Logger.Error("op timeout exceeds op-timeout-max; failing op",
			zap.String("stage", stage), zap.String("op", name))
		_, _ = pu.Runs.RecordTerminal(reqCtx, runID, stage, ordinal, name, "failed",
			failPayload("op timeout exceeds op-timeout-max"))
		return
	}

	// 'pending' step on the original (suspending) trace so the
	// barrier scope is visible there — same shape as remote async.
	aStart := time.Now()
	ack, _ := json.Marshal(map[string]string{"status": "accepted", "transport": "async-local"})
	trace.FromContext(reqCtx).Step(trace.StepInfo{
		Stack: op.Stack, Scope: op.Scope, Name: name,
		Operation: op.Resonator.Exec, Transport: "async",
		Input:     []byte(op.Input),
		Output:    ack,
		StartedAt: aStart, FinishedAt: time.Now(),
		Status: "pending",
	})

	// Detach. Fresh ctx (background) so the request ctx ending
	// after the 202 emit doesn't kill the in-flight op. The
	// per-op timeout still bounds the work.
	go func() {
		workCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Spawn a resume trace — symmetric with continuation.go's
		// callback handler. The actual MCP call, RecordTerminal,
		// and any subsequent Resume scopes all emit step events
		// to this trace, so admin-ui shows them under one rid
		// distinct from the suspending request's rid.
		var tracer trace.RequestTracer
		if pu.Sink != nil {
			tracer = pu.Sink.Begin(trace.RequestInfo{
				RID:       continuation.ResumeTraceRID(runID, stage),
				Src:       "continuation",
				Stack:     stage,
				StartedAt: time.Now(),
			})
			// Linkage event — admin-ui uses these to cross-navigate
			// from the original (suspending) request to the resume
			// trace and back.
			if rc, rcErr := pu.Runs.ReadRunCreated(workCtx, runID); rcErr == nil {
				tracer.Event(trace.TimelineEvent{
					Ts:    time.Now(),
					Event: "continuation.resume",
					Fields: map[string]any{
						"run_id":              runID,
						"run_continuation_id": rc.RunContinuationID,
						"origin_rid":          rc.OriginRID,
						"stage":               stage,
						"stack_version_id":    rc.StackVersionID,
					},
				})
			}
			workCtx = trace.WithContext(workCtx, tracer)
		}

		out, eerr := pu.Exec(workCtx, op)
		status := "completed"
		var payload string
		if eerr != nil {
			status = "failed"
			payload = string(failPayload(eerr.Error()))
		} else {
			payload = out.Raw
			if out.Type == event.Null || payload == "" {
				payload = "{}"
			}
			if op.Resonator.Emit != nil {
				out, oerr := pu.OverlayResponse(op.Input, payload, op.Resonator.Emit.Overrides)
				if oerr != nil {
					status = "failed"
					payload = string(failPayload(oerr.Error()))
				} else {
					payload = out
				}
			}
		}
		if _, terr := pu.Runs.RecordTerminal(workCtx, runID, stage, ordinal, name, status, []byte(payload)); terr != nil {
			pu.Logger.Error("local-async: RecordTerminal failed",
				zap.String("run", runID), zap.String("stage", stage),
				zap.String("op", name), zap.Error(terr))
			if tracer != nil {
				tracer.End("error", nil)
			}
			return
		}

		// Stage may have other async ops still pending — only
		// drive resume when this completion makes the stage
		// resumable. Symmetric with continuation.go's callback
		// handler.
		ss, sserr := pu.Runs.ReadStageSuspended(workCtx, runID, stage)
		if sserr != nil {
			if tracer != nil {
				tracer.End("error", nil)
			}
			return
		}
		state, _ := pu.Runs.StageState(workCtx, runID, stage, ss.Manifest)
		if state != continuation.StateResumable {
			// Sibling ops still pending — partial completion. The
			// other op's callback will drive resume.
			if tracer != nil {
				tracer.End("ok", nil)
			}
			return
		}
		won, _ := pu.Runs.ClaimResume(workCtx, runID, stage)
		if !won {
			if tracer != nil {
				tracer.End("ok", nil)
			}
			return
		}
		rerr := pu.Resume(workCtx, runID, stage)
		if rerr != nil {
			pu.Logger.Error("local-async: Resume failed",
				zap.String("run", runID), zap.String("stage", stage), zap.Error(rerr))
		}
		if tracer != nil {
			rStatus := "ok"
			var final []byte
			if rerr != nil {
				rStatus = "error"
			} else if res, ok, _ := pu.Runs.ReadResult(workCtx, runID); ok {
				final = res
			}
			tracer.End(rStatus, final)
		}
	}()
}

func (pu *Unit) scopeHasAsync(ops []operation.Operation) bool {
	for _, op := range ops {
		if isAsyncOp(op) {
			return true
		}
	}
	return false
}

// opIdentity is the stable per-scope op name used for ordinal sort and
// store keys. File-derived Name is the identity; legacy nameless rules
// fall back to OpID.
func opIdentity(op operation.Operation) string {
	if op.Name != "" {
		return op.Name
	}
	return op.OpID
}

func (pu *Unit) callbackURLFor(opc string) string {
	base := strings.TrimRight(pu.CallbackBaseURL, "/")
	if base == "" {
		host := pu.Conf.Fqdn
		if host == "" {
			host = "localhost"
		}
		base = "http://" + host + pu.Conf.WebAddr
	}
	return base + "/_txc/continuations/op/" + opc + "/complete"
}

func failPayload(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg}})
	return b
}

// suspendBarrierScope is the barrier path: persist ALL durable records
// (run, stage manifest, op-created + op-continuation lookups) BEFORE any
// worker is invoked, then dispatch (sync ops execute and record their
// terminal immediately; async ops POST their worker and record accept),
// then return 202 to the client. The run resumes later via the callback
// endpoint. Resume reuses advanceAfterScope (see Phase 3).
func (pu *Unit) suspendBarrierScope(ctx context.Context, raw string, ops []operation.Operation, resCh chan event.Payload) error {
	cstage := ops[0].Stack + "/" + strconv.Itoa(ops[0].Scope)
	stack := ops[0].Stack
	tenant, _ := ctx.Value(ctxKeyTenant).(string)

	// Deterministic ordinal = position after sorting the scope's ops by
	// identity. Sort an index slice so ops itself is untouched.
	order := make([]int, len(ops))
	for i := range ops {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return opIdentity(ops[order[a]]) < opIdentity(ops[order[b]])
	})

	// expires_at = max WITH max_duration among async ops (0 ⇒ no expiry
	// in v1; the sweeper is deferred).
	var maxDur time.Duration
	for _, op := range ops {
		if !isAsyncOp(op) {
			continue
		}
		if v := gjson.Get(op.Meta, "max_duration"); v.Exists() {
			if d, derr := time.ParseDuration(v.String()); derr == nil && d > maxDur {
				maxDur = d
			}
		}
	}
	var expiresAt time.Time
	expStr := ""
	if maxDur > 0 {
		expiresAt = time.Now().UTC().Add(maxDur)
		expStr = expiresAt.Format(time.RFC3339)
	}

	// Fresh request → create the run + client handle. Resume (a later
	// async barrier on the same run) → reuse the existing identity and
	// only add this stage's docs; no new run, no client 202.
	var runID, rcid string
	resuming := false
	// Snapshot hash recorded on the run/stage docs (debug/trace only —
	// resume loads the snapshot doc by runID, not by hash). Set on first
	// suspend; "" on a re-entered (resume) suspend of the same run.
	snapHash := ""
	if ri, ok := resumeRunFrom(ctx); ok {
		runID, rcid, resuming = ri.runID, ri.rcid, true
	} else if di, ok := deferredRunFrom(ctx); ok {
		// A deferred op was already dispatched on this request, so the run
		// (and its opstack snapshot) already exist. Reuse that identity for
		// this same-scope barrier rather than minting a second run. NOT
		// resuming: the client is still attached and this is its first 202.
		runID, rcid = di.runID, di.rcid
	} else {
		var err error
		// Freeze the opstack this run resolves against into an immutable
		// continuation doc. Resume rebuilds an in-memory DB from it and
		// runs the same lookup query, so a later `txco apply` cannot
		// change what this in-flight run executes. snapshotOpstack
		// failure (or an empty/untenanted stack) degrades to "" — resume
		// then falls back to the live opstack (back-compat).
		var snapData []byte
		var snapN int
		if d, h, n, serr := pu.snapshotOpstack(ctx, tenant); serr != nil {
			pu.Logger.Warn("opstack snapshot failed; run will resume against live opstack",
				zap.String("tenant", tenant), zap.String("stack", stack), zap.Error(serr))
		} else if n > 0 {
			snapData, snapHash, snapN = d, h, n
		}
		// originRID = the suspending request's trace rid (back-pointer
		// for admin-ui trace linking). stack_version_id carries the
		// opstack-snapshot content hash.
		originRID, _ := ctx.Value(config.CtxKeyRid).(string)
		runID, rcid, err = pu.Runs.CreateRun(ctx, tenant, stack, snapHash, cstage, originRID, expiresAt)
		if err != nil {
			return err
		}
		if snapN > 0 {
			if werr := pu.Runs.WriteOpstackSnapshot(ctx, runID, snapData); werr != nil {
				pu.Logger.Warn("opstack snapshot write failed; run will resume against live opstack",
					zap.String("run", runID), zap.Error(werr))
			}
		}
		_ = pu.Runs.AppendEvent(ctx, runID, "run.created", map[string]any{
			"stack": stack, "stage": cstage, "tenant": tenant,
		})
	}

	// Mark the suspend on the current request's trace so admin-ui can
	// link it to the resume trace(s). Cheap; NoopTracer when tracing off.
	trace.FromContext(ctx).Event(trace.TimelineEvent{
		Ts:    time.Now(),
		Event: "continuation.suspend",
		Fields: map[string]any{
			"run_id":              runID,
			"run_continuation_id": rcid,
			"stage":               cstage,
		},
	})

	type asyncCred struct {
		opc   string
		token string
	}
	creds := make(map[int]asyncCred) // ordinal → cred
	manifest := make([]continuation.OpManifestEntry, 0, len(order))
	specs := make([]continuation.OpRecordSpec, 0, len(order))
	for ordinal, idx := range order {
		op := ops[idx]
		name := opIdentity(op)
		async := isAsyncOp(op)
		in := op.Input
		if in == "" {
			in = "{}"
		}
		sp := continuation.OpRecordSpec{
			Ordinal: ordinal, Op: name, Async: async,
			Input: []byte(in), ExpiresAt: expiresAt,
		}
		if async {
			opc, oerr := continuation.NewOpContinuationID()
			if oerr != nil {
				return oerr
			}
			token, hash, terr := continuation.MintToken()
			if terr != nil {
				return terr
			}
			sp.OpContinuationID = opc
			sp.TokenHash = hash
			creds[ordinal] = asyncCred{opc: opc, token: token}
		}
		manifest = append(manifest, continuation.OpManifestEntry{Ordinal: ordinal, Op: name, Async: async})
		specs = append(specs, sp)
	}

	if err := pu.Runs.SuspendStage(ctx, runID, cstage, raw, snapHash, manifest); err != nil {
		return err
	}
	if err := pu.Runs.CreateOpRecords(ctx, runID, cstage, specs); err != nil {
		return err
	}
	_ = pu.Runs.AppendEvent(ctx, runID, "stage.suspended", map[string]any{
		"stage": cstage, "ops": len(manifest),
	})

	// Only now dispatch. Every durable record exists, so a worker that
	// calls back immediately (even before the 202 below) resolves fine.
	var wg sync.WaitGroup
	for ordinal, idx := range order {
		op := ops[idx]
		name := opIdentity(op)
		async := isAsyncOp(op)

		// Local async: detached from wg so the 202 emits immediately
		// without waiting for the (possibly slow) chassis-internal
		// op to complete. Records 'pending' on the suspending trace
		// inline, then spawns a background goroutine that runs the
		// op, writes its terminal, and triggers Resume when the
		// stage is fully complete.
		if async && isLocalAsyncOp(op) {
			pu.dispatchLocalAsync(ctx, op, runID, cstage, ordinal, name)
			continue
		}

		wg.Add(1)
		go func(ordinal int, op operation.Operation, name string, async bool) {
			defer wg.Done()
			timeout, over := pu.opMetaTimeout(op)
			if over {
				pu.Logger.Error("op timeout exceeds op-timeout-max; failing op",
					zap.String("stage", cstage), zap.String("op", name))
				_, _ = pu.Runs.RecordTerminal(ctx, runID, cstage, ordinal, name, "failed",
					failPayload("op timeout exceeds op-timeout-max"))
				return
			}
			octx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			if !async {
				out, eerr := pu.Exec(octx, op)
				if eerr != nil {
					_, _ = pu.Runs.RecordTerminal(ctx, runID, cstage, ordinal, name, "failed",
						failPayload(eerr.Error()))
					return
				}
				payload := out.Raw
				if out.Type == event.Null || payload == "" {
					payload = "{}"
				}
				if op.Resonator != nil && op.Resonator.Emit != nil {
					out, oerr := pu.OverlayResponse(op.Input, payload, op.Resonator.Emit.Overrides)
					if oerr != nil {
						_, _ = pu.Runs.RecordTerminal(ctx, runID, cstage, ordinal, name, "failed",
							failPayload(oerr.Error()))
						return
					}
					payload = out
				}
				_, _ = pu.Runs.RecordTerminal(ctx, runID, cstage, ordinal, name, "completed", []byte(payload))
				return
			}

			cr := creds[ordinal]
			env := AsyncEnvelope{
				OpContinuationID:  cr.opc,
				CallbackURL:       pu.callbackURLFor(cr.opc),
				RunID:             runID,
				RunContinuationID: rcid,
				Stack:             stack,
				Stage:             cstage,
				Op:                name,
				ExpiresAt:         expStr,
			}
			aStart := time.Now()
			jobID, aerr := pu.ExecHTTPAsync(octx, op, env, cr.token)
			if aerr != nil {
				pu.Logger.Warn("async dispatch failed",
					zap.String("stage", cstage), zap.String("op", name), zap.Error(aerr))
				trace.FromContext(octx).Step(trace.StepInfo{
					Stack: op.Stack, Scope: op.Scope, Name: name,
					Operation: op.Resonator.Exec, Transport: "async",
					Input:     []byte(op.Input),
					StartedAt: aStart, FinishedAt: time.Now(),
					Status: "error", Error: aerr.Error(),
				})
				_, _ = pu.Runs.RecordTerminal(ctx, runID, cstage, ordinal, name, "failed",
					failPayload(aerr.Error()))
				return
			}
			_ = pu.Runs.RecordAccepted(ctx, runID, cstage, ordinal, name, jobID)
			// Surface the async op on the suspending request's trace so
			// the barrier scope is visible (not a gap between the last
			// sync scope and request.end). Status "pending": dispatched,
			// awaiting the worker callback. The completed/failed result
			// is recorded on the resume trace (see Resume).
			ack, _ := json.Marshal(map[string]string{"status": "accepted", "job_id": jobID})
			trace.FromContext(octx).Step(trace.StepInfo{
				Stack: op.Stack, Scope: op.Scope, Name: name,
				Operation: op.Resonator.Exec, Transport: "async",
				Input:     []byte(op.Input),
				Output:    ack,
				StartedAt: aStart, FinishedAt: time.Now(),
				Status: "pending",
			})
		}(ordinal, op, name, async)
	}
	wg.Wait()

	// Under resume there is no client waiting (it already got its 202 on
	// the original request); emitting here would be misread by Resume's
	// capture channel as a final result. Re-suspend is silent.
	if resuming {
		return nil
	}

	pu.emitContinuation202(ctx, raw, rcid, resCh)
	return nil
}

// emitContinuation202 synthesizes and emits the client "still running"
// response for a suspended run. The poll/deferred-response URL is the same
// request path + ?_txc.continuation=<rcid> (the dedicated GET was removed;
// this marker form flows through the normal traced pipeline —
// detectTenantBody short-circuits it to txco://continuation-result).
//
// Shared by the same-scope barrier (suspendBarrierScope) and the
// deferred-join suspend path so both emit an identical 202/303.
func (pu *Unit) emitContinuation202(ctx context.Context, raw, rcid string, resCh chan event.Payload) {
	pollPath := gjson.Get(raw, "_txc.web.req.url.path").String()
	if pollPath == "" {
		pollPath = "/"
	}
	pollURL := pollPath + "?_txc.continuation=" + rcid
	accept := gjson.Get(raw, "_txc.web.req.headers.Accept.0").String()
	format := gjson.Get(raw, "_txc.web.req.url.query.format.0").String()
	out := raw

	if format != "json" && strings.Contains(accept, "text/html") {
		// Browser: a 202+JSON Location is NOT auto-followed by browsers,
		// so a human hitting the app would just see raw JSON. Redirect
		// (303 See Other) to the poll URL — the browser GETs it and
		// lands on the branded waiting page, which then polls itself.
		out, _ = sjson.Set(out, "_txc.web.res.status", 303)
		out, _ = sjson.Set(out, "_txc.web.res.headers.location.0", pollURL)
		out, _ = sjson.Set(out, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
		out, _ = sjson.Set(out, "_txc.web.res.headers.cache-control.0", "no-store")
		// Non-empty body: the inlet treats an empty _txc.web.res.body as
		// "no body → emit the whole envelope". The browser discards this
		// and follows Location.
		out, _ = sjson.Set(out, "_txc.web.res.body",
			base64.StdEncoding.EncodeToString([]byte("redirecting…\n")))
	} else {
		// Machine / curl / programmatic poller: 202 + JSON status +
		// Location header (parsed, not auto-followed). no-store: the
		// poll URL returns different content over time, a cached 202
		// would strand the client on "running" forever.
		out, _ = sjson.Set(out, "_txc.web.res.status", 202)
		out, _ = sjson.Set(out, "_txc.web.res.headers.location.0", pollURL)
		out, _ = sjson.Set(out, "_txc.web.res.headers.retry-after.0", "3")
		out, _ = sjson.Set(out, "_txc.web.res.headers.content-type.0", "application/json")
		out, _ = sjson.Set(out, "_txc.web.res.headers.cache-control.0", "no-store")
		cbody, _ := json.Marshal(map[string]string{"status": "running", "continuation": rcid})
		out, _ = sjson.Set(out, "_txc.web.res.body", base64.StdEncoding.EncodeToString(cbody))
	}

	select {
	case resCh <- event.Payload{Raw: out, Type: event.JSON}:
	case <-ctx.Done():
	}
}

// Resume merges all op-terminal outputs for (runID, stage) in ascending
// ordinal onto the stage's scope-entry envelope, then drives the opstack
// forward via advanceAfterScope (the same engine the sync path uses).
// Called by the winning continuation callback. Writes result.json on
// terminal completion; if the pipeline re-suspends at a later async
// barrier nothing is written here — that stage's callbacks drive it. A
// failed sibling op fails the stage (design: a failed sibling fails the
// scope). Runs in the caller's ctx, not the dead request ctx.
func (pu *Unit) Resume(ctx context.Context, runID, stage string) error {
	ss, err := pu.Runs.ReadStageSuspended(ctx, runID, stage)
	if err != nil {
		return err
	}

	// Deferred-join resume diverges from the same-scope barrier. A same-scope
	// barrier's manifest entries ARE the suspended scope's own ops (they ran
	// as the scope), so the resume merges their terminals and calls
	// advanceAfterScope (post-scope decision). A deferred-join suspend's
	// manifest entries are ops dispatched at an EARLIER scope; the join
	// scope's own ops (the SELECT … that consumes the result) have NOT run.
	// So the deferred path merges the deferred terminals then RE-RUNS the
	// join scope. Detected by manifest entries carrying OpContinuationID.
	if isDeferredJoinManifest(ss.Manifest) {
		return pu.resumeDeferredJoin(ctx, runID, stage, ss)
	}

	merged := ss.ScopeEnvelope
	if merged == "" {
		merged = "{}"
	}

	// Parsed once for the per-op trace steps below, so the barrier scope
	// (e.g. 300) shows on the resume trace with each async op's worker
	// result — the other side of the suspend-trace's "pending" steps.
	rStack, rScope, _ := pu.StageParse(stage)

	entries := append([]continuation.OpManifestEntry(nil), ss.Manifest...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Ordinal < entries[j].Ordinal })
	for _, e := range entries {
		term, terr := pu.Runs.ReadOpTerminal(ctx, runID, stage, e.Ordinal, e.Op)
		if terr != nil {
			return terr
		}
		if term.Status == "failed" {
			var eb []byte
			if term.ErrorKey != "" {
				eb, _ = pu.Runs.Get(ctx, term.ErrorKey)
			}
			trace.FromContext(ctx).Step(trace.StepInfo{
				Stack: rStack, Scope: rScope, Name: e.Op, Transport: "async",
				Output: eb, StartedAt: ss.SuspendedAt, FinishedAt: term.RecordedAt,
				Status: "error", Error: "op failed",
			})
			_ = pu.Runs.FailStage(ctx, runID, stage, "op-failed:"+e.Op)
			_ = pu.Runs.AppendEvent(ctx, runID, "stage.failed",
				map[string]any{"stage": stage, "op": e.Op})
			return nil
		}
		var ob []byte
		if term.OutputKey != "" {
			var gerr error
			ob, gerr = pu.Runs.Get(ctx, term.OutputKey)
			if gerr != nil {
				return gerr
			}
		}
		trace.FromContext(ctx).Step(trace.StepInfo{
			Stack: rStack, Scope: rScope, Name: e.Op, Transport: "async",
			Output: ob, StartedAt: ss.SuspendedAt, FinishedAt: term.RecordedAt,
			Status: "completed",
		})
		if s := string(ob); s != "" && s != "{}" {
			if m, merr := pu.MergeJSON(merged, s); merr == nil {
				merged = m
			} else {
				pu.Logger.Warn("resume merge error",
					zap.String("run", runID), zap.String("op", e.Op), zap.Error(merr))
			}
		}
	}

	// Reconstruct ops/nextOps exactly as Run's prelude does, against the
	// merged envelope. rctx carries the run identity so a later async
	// barrier reuses this run (multi-stage) instead of minting a new one.
	//
	// Pin the tenant from the suspended SCOPE-ENTRY envelope, mirroring
	// Run's pin (the `ctxKeyTenant` set at processor.go:278 from
	// `_txc.tenant`). Resume is called with the web controller's ctx,
	// which has no tenant pinned; without this OpsForStage is scoped to
	// "" and finds zero ops, so the run completes stuck at the barrier
	// scope.
	//
	// SECURITY: read from ss.ScopeEnvelope (the chassis-stamped envelope
	// captured at suspend, BEFORE any worker output was merged), NOT from
	// `merged`. `merged` has the workers' op outputs deep-merged in; a
	// hostile/buggy worker returning {"_txc":{"tenant":"…"}} would
	// otherwise overwrite the pin and escape into another tenant's
	// stacks. The tenant must come only from chassis-origin data, never
	// from worker-supplied output — same invariant Run enforces by
	// pinning once from the original `raw`.
	rctx := withResumeRun(ctx, runID, "")
	rctx = WithTenant(rctx, gjson.Get(ss.ScopeEnvelope, "_txc.tenant").String())

	// Resume against the opstack frozen at suspend, not the live one. The
	// snapshot DB is attached to rctx so EVERY opstack lookup in the
	// resumed execution — this stage, nextOps, and any deeper
	// goto/next-stage advancement or re-suspend — resolves against it
	// (opstackDB(ctx) reads ctxKeyOpstackSnap). Absent/empty snapshot ⇒
	// no override ⇒ falls back to the live opstack (back-compat for
	// pre-snapshot runs and untenanted/_sys). Held for the whole
	// synchronous resume; closed when it returns.
	if snapData, serr := pu.Runs.ReadOpstackSnapshot(ctx, runID); serr == nil && len(snapData) > 0 {
		snapDB, berr := buildSnapshotDB(snapData)
		if berr != nil {
			pu.Logger.Warn("opstack snapshot rebuild failed; resuming against live opstack",
				zap.String("run", runID), zap.Error(berr))
		} else {
			defer snapDB.Close()
			rctx = context.WithValue(rctx, ctxKeyOpstackSnap, snapDB)
		}
	}

	ops, err := pu.OpsForStage(rctx, stage)
	if err != nil {
		return err
	}
	ops, err = pu.ResonatingOps(merged, ops, "")
	if err != nil {
		return err
	}
	nextOps, _ := pu.OpsForStage(rctx, pu.nextStageFor(stage, ""))

	capCh := make(chan event.Payload, 1)
	opsDone := false
	if _, aerr := pu.advanceAfterScope(rctx, stage, merged, ops, "", nextOps, &opsDone, capCh, func() {}); aerr != nil {
		return aerr
	}

	select {
	case p := <-capCh:
		// A payload here is a genuine terminal result (halt/default, or a
		// synchronous continuation that ran to completion). Re-suspend is
		// silent under resume, so this is never a 202.
		if werr := pu.Runs.WriteResult(ctx, runID, []byte(p.Raw)); werr != nil {
			return werr
		}
		_ = pu.Runs.AppendEvent(ctx, runID, "run.completed", map[string]any{"stage": stage})
	default:
		// Re-suspended at a later async barrier; the new stage's docs
		// (and its callbacks) drive the run forward.
	}
	return nil
}

// MergeJSON iterate through top level keys, add them to other struct, return it
func (pu *Unit) MergeJSON(src string, dst string) (string, error) {
	// doesn't deep merge. maybe https://play.golang.org/p/8jlJUbEJKf ?

	// short circuit for empty strings
	if dst == "" {
		return src, nil
	}
	if src == "" {
		return dst, nil
	}

	d := gjson.Parse(dst)
	s := gjson.Parse(src)
	if !d.IsObject() || !s.IsObject() {
		return "", errors.New("Merging requires both sides to be objects")
	}

	var escapeDot = func(key string) string {
		s := strings.ReplaceAll(key, ".", "\\.")
		return strings.ReplaceAll(s, ":", "\\:")
	}

	var deepmerge func(x1, x2 interface{}) interface{}
	deepmerge = func(x1, x2 interface{}) interface{} {
		switch x1 := x1.(type) {
		case map[string]interface{}:
			x2, ok := x2.(map[string]interface{})
			if !ok {
				return x1
			}
			for k, v2 := range x2 {
				if v1, ok := x1[k]; ok {
					x1[k] = deepmerge(v1, v2)
				} else {
					x1[k] = v2
				}
			}
		case nil:
			// merge(nil, map[string]interface{...}) -> map[string]interface{...}
			x2, ok := x2.(map[string]interface{})
			if ok {
				return x2
			}
		case []interface{}:
			x2, ok := x2.([]interface{})
			if ok {
				merged := append(x2, x1...)
				return merged
			}
			return x2
		}
		return x1
	}

	var derr error
	d.ForEach(func(key, value gjson.Result) bool {
		// if the typeof the value is not object or map, just insert
		// otherwise, we merge insert
		switch value.Type {
		case gjson.JSON:
			// see if this object exists in both source and destination
			v := gjson.Get(src, escapeDot(key.String()))
			if !v.Exists() {
				// good news, we can just insert it
				switch {
				case value.IsObject():
					m, ok := value.Value().(map[string]interface{})
					if !ok {
						derr = errors.New("merge object error")
						return false
					}
					src, _ = sjson.Set(src, escapeDot(key.String()), m)
				case value.IsArray():
					m, ok := value.Value().([]interface{})
					if !ok {
						derr = errors.New("merge array error")
						return false
					}
					src, _ = sjson.Set(src, escapeDot(key.String()), m)
				}
			} else {
				// exists in both places, let's deepmerge, append arrays
				switch {
				case value.IsObject():
					d1, dok := value.Value().(map[string]interface{})
					s1, sok := v.Value().(map[string]interface{})
					if !dok || !sok {
						derr = errors.New("deepmerge obj error")

						return false
					}

					merged := deepmerge(d1, s1)
					src, _ = sjson.Set(src, escapeDot(key.String()), merged)
				case value.IsArray():
					d1, dok := value.Value().([]interface{})
					s1, sok := v.Value().([]interface{})
					if v.IsArray() {
						if !dok || !sok {
							derr = errors.New("deepmerge obj error")
							return false
						}
						// TODO: check to see if arrays v/value are the same size and if s1 == d1.
						// if it's the same, don't append.
						merged := append(s1, d1...)
						src, _ = sjson.Set(src, escapeDot(key.String()), merged)
					} else {
						src, _ = sjson.Set(src, escapeDot(key.String()), d1)
					}
				}
			}

		case gjson.String:
			src, _ = sjson.Set(src, escapeDot(key.String()), value.String())
		case gjson.False:
			src, _ = sjson.Set(src, escapeDot(key.String()), false)
		case gjson.True:
			src, _ = sjson.Set(src, escapeDot(key.String()), true)
		case gjson.Number:
			src, _ = sjson.Set(src, escapeDot(key.String()), value.Num)
		default:
			src, _ = sjson.Set(src, escapeDot(key.String()), nil)
		}

		return true // keep iterating
	})

	return src, derr
}

// ExecCore on core execution (vs remote)
func (pu *Unit) ExecCore(ctx context.Context, op operation.Operation) (event.Payload, error) {
	opName := op.Resonator.Exec
	pu.Logger.Debug("ExecCore", zap.String("opname", opName))

	root := pu.Mux.Root()
	_, handler, found := root.LongestPrefix([]byte(opName))
	if !found {
		err := errors.New("op not found in core")
		return event.Payload{
			Raw: `{}`,
			//		Raw:  string(opstack),
			Type: event.Null,
			Meta: `{"error":["dial-core-lookup-err"]}`,
		}, err
	}

	var out []byte
	in := []byte(op.Input)

	// Plumb the per-op SecretBag + Meta onto ctx so chassis-core
	// handlers (txco://hmac-sign, txco://basic-auth-encode, future
	// signing ops) can read materialized cleartext AND their WITH-
	// clause parameters. ExecHTTP gets `op` directly; core handlers
	// only get (ctx, opName, in, out), so both travel via ctx.
	if op.Secrets.Len() > 0 {
		ctx = secrets.WithBag(ctx, &op.Secrets)
	}
	if op.Meta != "" {
		ctx = operation.WithMeta(ctx, op.Meta)
	}

	ep, err := handler.(event.OpsHandler).Route(ctx, opName, in, out)
	if err != nil {
		pu.Logger.Debug("event payload error", zap.String("err", err.Error()))
		return event.Payload{
			Raw: `{}`,
			//		Raw:  string(opstack),
			Type: event.Null,
			Meta: `{"error":["processing-err"]}`,
		}, err
	}
	return ep, err
}

// dispatchesToHandler reports whether the EXEC operand will cause the
// chassis to hand the envelope to a handler — local or remote, any
// transport. Stage jumps stay inside the chassis's routing machinery and
// don't dispatch; unsupported schemes error out before reaching a
// handler. Both skip envelope injection.
func dispatchesToHandler(opName string) bool {
	switch {
	case strings.HasPrefix(opName, "txco://"):
		return true
	case strings.HasPrefix(opName, "http://"):
		return true
	case strings.HasPrefix(opName, "https://"):
		return true
	case strings.HasPrefix(opName, "compute://"):
		return true
	}
	return false
}

// shouldMockByPattern reads `_txc.mocks` from the envelope and reports
// whether the firing op's canonical id (`<stack>/<scope>/<name>`)
// matches one of the patterns. Forward-slash globs with `!` prefix for
// exclusion; semantics come from chassis/utils/filematch (last match
// wins). Accepts a single string OR an array of strings for ergonomics.
// Returns false if `_txc.mocks` is absent, malformed, or empty.
func (pu *Unit) shouldMockByPattern(op operation.Operation) bool {
	raw := gjson.Get(op.Input, "_txc.mocks")
	if !raw.Exists() {
		return false
	}
	var patterns []string
	if raw.IsArray() {
		raw.ForEach(func(_, v gjson.Result) bool {
			if s := v.String(); s != "" {
				patterns = append(patterns, s)
			}
			return true
		})
	} else if raw.Type == gjson.String {
		if s := raw.String(); s != "" {
			patterns = []string{s}
		}
	}
	if len(patterns) == 0 {
		return false
	}
	pm, err := filematch.NewPatternMatcher(patterns)
	if err != nil {
		pu.Logger.Warn("_txc.mocks pattern parse failed", zap.Error(err))
		return false
	}
	id := fmt.Sprintf("%s/%d/%s", op.Stack, op.Scope, op.Name)
	ok, _ := pm.Matches(id)
	return ok
}

// injectRuntimeIdentity stamps the firing rule's identity onto the
// envelope bytes that are about to be handed to a handler.
//
//	_txc.op   = "<stack>/<name>"   (e.g. "hello-world/canary/hello")
//	_txc.step = <scope>            (integer)
//
// Shape mirrors OLD's runtime operation name ($Env-$Service-$Slot-$OpName)
// minus step (its own field) and minus environment (not strongly modeled
// in v2). Slot is already baked into stack via the prefix-fallback
// overlay, so slot-canary rules naturally produce "stack/canary/name".
// Already-present values are overwritten — the rule firing now is
// authoritative; stale identity from a prior stage must not leak.
func injectRuntimeIdentity(body, stack, name string, scope int) string {
	if body == "" {
		body = "{}"
	}
	body, _ = sjson.Set(body, "_txc.op", stack+"/"+name)
	body, _ = sjson.Set(body, "_txc.step", scope)
	return body
}

// Exec Execute an operation at this step
func (pu *Unit) Exec(ctx context.Context, op operation.Operation) (event.Payload, error) {

	execStart := time.Now()

	// are we invoking an operation, or changing steps?
	opName := op.Resonator.Exec
	pu.Logger.Debug("Exec", zap.String("opname", opName))

	ctx, span := pu.Mc.Tracer.Start(ctx, `exec-`+opName)
	defer span.End()

	// Stamp the firing rule's identity on the envelope before any handler
	// sees it. Transport-agnostic: every dispatching branch (txco://, HTTP
	// today, future schemes like gRPC) shares this one site, so adding a
	// new transport below automatically inherits the behavior. Stage jumps
	// and unsupported schemes don't dispatch to a handler and are skipped.
	if dispatchesToHandler(opName) {
		op.Input = injectRuntimeIdentity(op.Input, op.Stack, op.Name, op.Scope)
	}

	var payload event.Payload
	var err error = nil
	var transport string

	// Caller-driven mock interception. If the inbound envelope set
	// `_txc.mocks` to a list of glob patterns and this op's canonical
	// id (`<stack>/<scope>/<name>`) matches one, short-circuit to the
	// stored mock_res. Empty mock_res → fall through to real exec;
	// the caller said "mock these" but the rule author didn't supply
	// a fixture, so running real beats silently returning {}.
	mockByPattern := pu.shouldMockByPattern(op) && len(op.MockRes) > 0

	switch {
	case mockByPattern:
		payload = pu.MakeMockResponse(op, "_txc.mocks-pattern-match")
		transport = "mock"
	case opName == "":
		// Rule with no EXEC clause — same shape as `EXEC "txco://noop"`.
		// Returns `{}` so EMIT (if any) has something to overlay onto
		// and the merge sees this op like any other. Symmetric with the
		// explicit noop case: trace step still recorded, _txc.mocks
		// still considered above.
		payload = event.Payload{Raw: `{}`, Type: event.JSON}
		transport = "noop"
	// Scheme-prefixed forms first so a URL ending in /<digits> (e.g.
	// "http://example.com/op/100") isn't mis-classified as a stage jump.
	case strings.HasPrefix(opName, "txco://mock"):
		// Rule-author-driven mock interception. Loud failure on missing
		// mock_res so the author notices: they explicitly opted in.
		if len(op.MockRes) == 0 {
			err = fmt.Errorf("txco://mock used but no mock_res defined for %s/%d/%s",
				op.Stack, op.Scope, op.Name)
			payload = event.Payload{Raw: `{}`, Type: event.JSON}
		} else {
			payload = pu.MakeMockResponse(op, "txco://mock")
		}
		transport = "mock"
	case strings.HasPrefix(opName, "txco://"):
		payload, err = pu.ExecCore(ctx, op)
		transport = "txco"
	case strings.HasPrefix(opName, "http://"):
		payload, err = pu.ExecHTTP(ctx, op)
		transport = "http"
	case strings.HasPrefix(opName, "https://"):
		payload, err = pu.ExecHTTP(ctx, op)
		transport = "https"
	case strings.HasPrefix(opName, "compute://"):
		// Sandboxed local compute. `op://name` is resolved at apply time to
		// `compute://<alg>/<digest>`; here we load the content-addressed
		// artifact and run it on its engine. Output flows the identical
		// post-EXEC path (EMIT overlay, merge, _txc.goto/halt) as the other
		// transports — see the shared tail below + advanceAfterScope.
		payload, err = pu.ExecCompute(ctx, op)
		transport = "compute"
	case strings.HasPrefix(opName, "mcp+http://"),
		strings.HasPrefix(opName, "mcp+https://"):
		// MCP-over-HTTP: framed JSON-RPC `tools/call`. URL fragment
		// carries the tool name; the per-op session lifecycle
		// (initialize → notifications/initialized → tools/call) is
		// performed every call in v0 (correct, not optimized).
		payload, err = pu.ExecMCPHTTP(ctx, op)
		transport = "mcp+http"
	case strings.HasPrefix(opName, "goto://"): // TODO
	case StagePartsRE.MatchString(opName):
		// Unschemed `EXEC "<stack>/<scope>"` is a stage jump. We
		// synthesize a JSON response that sets `_txc.goto` so the
		// post-stage merge picks up the jump via the same machinery
		// that handles `_txc.goto` from any other op response. This
		// is the canonical "boot → service" pattern: a boot stack
		// fires once, EXECs into the service's stack, and the chassis
		// continues there.
		payload = event.Payload{
			Raw:  fmt.Sprintf(`{"_txc":{"goto":"%s"}}`, opName),
			Type: event.JSON,
		}
		transport = "goto"
	default:
		// gRPC was removed in this revision. Any non-recognized scheme is a
		// rule authoring error — fail loudly so it's spotted at first match
		// rather than silently dispatched somewhere.
		err = errors.New(`unsupported EXEC value; use "txco://...", "http(s)://...", "mcp+http(s)://host/path#tool", or a stage like "stack/scope"`)
		payload = pu.MakeMockResponse(op, "unsupported-scheme")
		transport = "unsupported"
	}

	execTime := int64(time.Since(execStart) / time.Millisecond)
	span.SetAttributes(attribute.String("transport", transport))

	rid, ok := ctx.Value(config.CtxKeyRid).(string)
	if (!ok) || (rid == "") {
		rid = "unset" // TODO: an error?
	}

	// see if we should log this, and if so how
	if pu.Conf.LogOps != "" {
		go func() {
			err := logging.WriteOps(pu.Logger, &pu.Conf, rid, opName, op, payload, execTime)
			if err != nil {
				// TODO: error handling
				pu.Logger.Error("WriteOps error", zap.String("err", err.Error()))
			}
		}()
	}

	// record exec time if we need to
	if len(pu.Conf.OpMetricsRegex) > 0 {
		opRE := regexp.MustCompile(pu.Conf.OpMetricsRegex)
		if opRE.MatchString(opName) {
			pu.Mc.RecordOp(ctx, opName, execTime)
		}
	}

	if err != nil {
		pu.Logger.Debug("Exec error", zap.String("err", err.Error()))
	}

	// Record this step to the trace sink. Same chokepoint as the
	// _txc.op/_txc.step injection (per feedback_http_local_symmetry):
	// HTTP, txco://, stage-jump, and unsupported branches all produce
	// identical trace shape. NoopTracer makes this a no-op when
	// tracing is disabled.
	finishedAt := time.Now()
	stepStatus := "ok"
	stepErr := ""
	if err != nil {
		stepStatus = "error"
		stepErr = err.Error()
	}
	// Trace the op's output AS THE OP PRODUCED IT — i.e. with any EMIT
	// overlay applied. The caller (Run / the deferred paths) applies the
	// same OverlayResponse to op.Output before the per-scope merge; we
	// mirror it here so the step shows the op's actual contribution. An
	// EMIT-only op returns `{}` from exec, so without this the step's
	// output would be empty and hide the emitted fields. Trace-only: the
	// returned `payload` is unchanged — the caller still owns the overlay
	// that feeds the merge.
	tracedOutput := payload.Raw
	if err == nil && op.Resonator != nil && op.Resonator.Emit != nil {
		base := payload.Raw
		if payload.Type == event.Null || base == "" {
			base = "{}"
		}
		if ov, oerr := pu.OverlayResponse(op.Input, base, op.Resonator.Emit.Overrides); oerr == nil {
			tracedOutput = ov
		}
	}
	trace.FromContext(ctx).Step(trace.StepInfo{
		Stack:      op.Stack,
		Scope:      op.Scope,
		Name:       op.Name,
		Operation:  opName,
		Transport:  transport,
		Txcl:       op.Txcl,
		Input:      []byte(op.Input),
		Output:     []byte(tracedOutput),
		StartedAt:  execStart,
		FinishedAt: finishedAt,
		Status:     stepStatus,
		Error:      stepErr,
	})

	return payload, err
}

// computeLogWriter routes a sandboxed compute's diagnostic output (console.*,
// written to the guest's stderr) to the chassis logger, tagged with the op id.
type computeLogWriter struct {
	logger *zap.Logger
	id     string
}

func (w computeLogWriter) Write(p []byte) (int, error) {
	if w.logger != nil {
		w.logger.Info("compute log", zap.String("op", w.id),
			zap.String("msg", strings.TrimRight(string(p), "\n")))
	}
	return len(p), nil
}

// ExecCompute runs a `compute://<alg>/<digest>` op: it resolves the
// content-addressed artifact and invokes it on its engine, returning the
// output as a JSON payload. The output is handed back to Exec's shared tail,
// so it flows the identical post-EXEC processing (EMIT overlay, per-scope
// merge, _txc.goto/halt) as http:// and txco:// — the compute transport is
// not a special case downstream.
func (pu *Unit) ExecCompute(ctx context.Context, op operation.Operation) (event.Payload, error) {
	ref, ok := compute.ParseRef(op.Resonator.Exec)
	if !ok {
		return event.Payload{Raw: `{}`, Type: event.JSON},
			fmt.Errorf("malformed compute ref %q; want compute://<alg>/<digest>", op.Resonator.Exec)
	}
	if pu.Computes == nil {
		return event.Payload{Raw: `{}`, Type: event.JSON},
			errors.New("compute:// op fired but no compute runtime configured")
	}
	// Seed the guest's wall clock with the real time at the moment this op
	// runs, so Date.now() reflects "now". (The request-ingress time is still
	// available to the compute as the envelope's _ts.)
	rctx := compute.WithNow(ctx, time.Now())
	opID := fmt.Sprintf("%s/%d/%s", op.Stack, op.Scope, op.Name)
	// Route the guest's console.* output to the chassis logger so it's visible.
	if pu.Logger != nil {
		rctx = compute.WithLogWriter(rctx, computeLogWriter{logger: pu.Logger, id: opID})
	}
	// Capture per-invocation runtime metrics (wall/memory/status) so they can
	// ride the usage event emitted after Run returns. (No separate "compute
	// metrics" log line / trace event — the usage line below carries the same
	// data plus tenant + bytes, so a second sink would just be duplication.)
	var cm compute.Metrics
	rctx = compute.WithMetricsSink(rctx, func(m compute.Metrics) { cm = m })

	rid, _ := ctx.Value(config.CtxKeyRid).(string)
	out, err := pu.Computes.Run(rctx, ref, computeStdin(op, opID, rid))

	// Usage event (src="compute"), mirroring the per-request usage line so
	// compute cost shows up in the same stream — keyed by tenant + the
	// compute's stack/scope/name.
	if pu.Usage != nil {
		ustatus := "ok"
		if err != nil {
			ustatus = "error"
		}
		pu.Usage.WriteEvent(usage.UsageEvent{
			RID:        rid,
			Tenant:     tenantScope(ctx),
			Src:        "compute",
			Stack:      opID,
			DurationMS: cm.WallMS,
			Status:     ustatus,
			BytesIn:    len(op.Input),
			BytesOut:   len(out),
			MemBytes:   int(cm.MemoryBytes),
		})
	}
	if err != nil {
		return event.Payload{Raw: `{}`, Type: event.JSON}, err
	}
	raw := string(out)
	if raw == "" {
		raw = "{}"
	}
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// computeStdin builds the ABI-v2 stdin envelope a compute reads:
//
//	{ "input": <op input>, "meta": {…}, "env": <op config>, "secrets": {NAME: cleartext} }
//
// stdout stays the bare output envelope so downstream merge/EMIT/goto are
// transport-agnostic. meta is the trace identity; env is the op's non-secret
// config — the WITH-clause channel (op.Meta), the one per-op config already
// plumbed to runtime, so @txco/op's ctx.env reads the same value an http://
// worker would receive in its meta. secrets carries the per-op materialized
// secrets by name — the same SecretBag an http:// op splices into its request,
// here handed to the in-process compute as ctx.secrets.<NAME>. (Reading the bag
// via Get is the deliberate, documented extraction path — the bag's panic-on-
// marshal guard stops cleartext reaching any envelope/trace/log/continuation,
// not this transient sandbox stdin.)
func computeStdin(op operation.Operation, opID, rid string) []byte {
	var input any = map[string]any{}
	if s := strings.TrimSpace(op.Input); s != "" {
		if json.Unmarshal([]byte(s), &input) != nil {
			input = op.Input // not JSON → pass the raw string through
		}
	}
	var env any = map[string]any{}
	if s := strings.TrimSpace(op.Meta); s != "" {
		_ = json.Unmarshal([]byte(s), &env) // best-effort; leave {} on failure
	}
	secretsMap := map[string]string{}
	for _, name := range op.Secrets.Names() {
		if v, ok := op.Secrets.Get(name); ok {
			secretsMap[name] = string(v)
		}
	}
	b, _ := json.Marshal(map[string]any{
		"input": input,
		"meta": map[string]any{
			"rid": rid, "op": opID, "stack": op.Stack, "scope": op.Scope, "name": op.Name,
		},
		"env":     env,
		"secrets": secretsMap,
	})
	return b
}

// NodeForOp Given an operation description, find matching node.
//
// Now that gRPC is removed and HTTP rules dispatch directly to op.Resonator.Exec,
// this is only consulted for txco:// (local) operations that route by the fixed
// registry. Kept around because the ExecCore path may still want logical-name
// resolution in the future; today it's effectively a passthrough.
func (pu *Unit) NodeForOp(ctx context.Context, op operation.Operation) (*registry.Node, error) {
	_, span := pu.Mc.Tracer.Start(ctx, `nodeforop`)
	defer span.End()

	return pu.Reg.NodeByFixed(op)
}

// resolveGoto turns the raw `_txc.goto` value into a fully-qualified stage.
// A value containing a slash (e.g. "boot/foo/3") is treated as already
// fully-qualified. A bare number (e.g. "3") is interpreted as a scope
// within the current stack. An empty value or an unparseable current stage
// returns "".
func (pu *Unit) resolveGoto(currentStage, raw string) string {
	if raw == "" {
		return ""
	}
	if StagePartsRE.MatchString(raw) {
		return raw
	}
	stack, _, err := pu.StageParse(currentStage)
	if err != nil {
		pu.Logger.Warn("goto resolve: bad current stage", zap.String("err", err.Error()))
		return ""
	}
	return stack + "/" + raw
}

// nextStageFor returns the stage to run next: the explicit goto if one was
// supplied, otherwise the current scope advanced by one.
func (pu *Unit) nextStageFor(currentStage, gotoStage string) string {
	if gotoStage != "" {
		pu.Logger.Debug("going to", zap.String("gotostage", gotoStage))
		return gotoStage
	}
	stack, scope, err := pu.StageParse(currentStage)
	if err != nil {
		pu.Logger.Warn("stage parse err", zap.String("err", err.Error()))
		return ""
	}
	nextStage := stack + "/" + strconv.Itoa(scope+1)
	pu.Logger.Debug("NEXT", zap.String("stage", nextStage))
	return nextStage
}

func (pu *Unit) StageParse(stage string) (stack string, scope int, err error) {

	if StagePartsRE.MatchString(stage) {
		matches := StagePartsRE.FindStringSubmatch(stage)

		if len(matches) > 0 {
			stack = matches[1]
			scope, err = strconv.Atoi(matches[2])
		} else {
			err = errors.New("invalid stage")
		}
	} else {
		err = errors.New("invalid stage, doesn't match: " + stage)
	}

	if err != nil {
		pu.Logger.Debug("StageParse error", zap.String("err", err.Error()))
	}

	return stack, scope, err
}

// ResonatingOps returns the subset of ops whose WHEN clause matches the input.
// If any matching op has Priority > 0, only the highest-priority op is returned;
// otherwise all matching ops are returned (parallel execution at this stage).
//
// Clause evaluation order on a single rule: WHEN, PRE-SET, SELECT, POST-SET,
// WITH, PRIORITY, EXEC. PRE-SET, SELECT, and POST-SET control the event payload
// passed to dispatch; WITH carries per-call metadata; PRIORITY tie-breaks
// matching rules; EXEC is the dispatch target.
func (pu *Unit) ResonatingOps(input string, ops []operation.Operation, hashSeed string) ([]operation.Operation, error) {

	highestPriority := int64(0)

	// short circuit, no ops
	if len(ops) == 0 {
		return ops, nil
	}

	// iterate through ops, convert txcl to resonator, evaluate resonator for input
	var i = 0

	for _, op := range ops {
		// get resonator for op.Txcl
		if op.Resonator == nil {
			res, errs := txcl.Resonator(op.Txcl)
			if res == nil || errs != nil {
				if errs != nil {
					pu.Logger.Debug("res error", zap.String("err", errs.Error()))
				}
				return ops, errs
			}
			op.Resonator = res
		}

		if op.Resonator.WhenMatches(input) {
			op.Input = input
			ops[i] = op
			i++

			if op.Resonator.Priority > highestPriority {
				highestPriority = op.Resonator.Priority
			}
		}
	}

	if i != 0 && len(ops) != 0 {
		// resize to be matches only
		ops = ops[:i]
	} else {
		return nil, nil
	}

	if highestPriority != 0 {
		// choose resonator with the highest priority
		// if any operation in the stage has a priority, then we return only one op
		// otherwise we return all
		i = 0
		for _, op := range ops {
			if op.Resonator.Priority == highestPriority {
				ops[i] = op
				i++
			}
		}
		if i != 0 && len(ops) != 0 {
			ops = ops[:i]
		}

		// if we have more than one match and are using priority, use hashSeed(RID?) to pick
		if len(ops) > 1 && len(hashSeed) != 0 {
			h := fnv.New32a()
			h.Write([]byte(hashSeed)) // nolint: errcheck
			shuffle := h.Sum32()

			ops[0] = ops[shuffle%uint32(len(ops))]
		}
		ops = ops[:0]
	}

	// decorate matching ops
	for j, op := range ops {

		// PRE-SET
		// debug, _ := json.MarshalIndent(best, "", " ")

		// fmt.Printf("resonator %s\n\n", debug)

		if op.Resonator.SetPre != nil {
			out, derr := pu.DecorateInput(op.Input, op.Resonator.SetPre.Overrides)
			if derr != nil {
				// SET PRE resolution failed for this op. Strict-by-default:
				// the op's Input becomes a failure payload so its EXEC sees
				// the error (and most ops will short-circuit to noop on a
				// payload they can't interpret). PR 2 can't trigger this
				// path; PR 3 onward may prefer to also short-circuit the
				// EXEC dispatch here.
				pu.Logger.Debug("decorate pre", zap.String("op", op.Name), zap.String("err", derr.Error()))
				op.Input = string(failPayload(derr.Error()))
			} else {
				op.Input = out
			}
		}

		if op.Resonator.With != nil {
			// Build op.Meta via sjson.Set so dotted keys
			// (`secrets.headers.authorization.secret`) explode into
			// nested objects, matching what gjson reads on the
			// consumer side. A plain json.Marshal would produce a
			// flat map with literal-dot keys — breaking gjson path
			// navigation and the secrets walker (see
			// chassis/secrets/refs.go and internal docs/todo-secret-store.md
			// §4). Flat keys (`timeout = 1000`) round-trip
			// byte-for-byte; only dotted keys differ in shape.
			meta := "{}"
			// Resolve every WITH value against the current op.Input
			// envelope. PR 2 only handles ast.Literal; PR 3 wires
			// FunctionCall so e.g. `WITH timeout = &concat(@a,@b)`
			// flows through the same code path.
			envForWith := runtime.JSONEnv(op.Input)
			unwrappedWith := make(map[string]interface{}, len(op.Resonator.With))
			withErr := false
			for k, v := range op.Resonator.With {
				resolved, rerr := runtime.Resolve(v, envForWith)
				if rerr != nil {
					pu.Logger.Debug("with resolve", zap.String("key", k), zap.String("err", rerr.Error()))
					withErr = true
					break
				}
				unwrappedWith[k] = resolved
			}
			if !withErr {
				for k, v := range unwrappedWith {
					var serr error
					meta, serr = sjson.Set(meta, k, v)
					if serr != nil {
						// Fall back to flat marshal for this op so
						// existing semantics aren't worse than before.
						if withData, jerr := json.Marshal(unwrappedWith); jerr == nil {
							meta = string(withData)
						}
						break
					}
				}
			}
			op.Meta = meta
		}

		// SELECT (path-copy with optional DEFAULT).
		// For each `<src> AS <dst> [DEFAULT <lit>]` assignment:
		//   1. Read From off the envelope view (op.Input). When the
		//      source path is missing or resolves to "", substitute
		//      Default if one was supplied.
		//   2. Pre-overlay: write the value into op.Input so the
		//      rule's EXEC sees it on its input view.
		//   3. Post-overlay: synthesize an EMIT entry so the writes
		//      persist into the merged scope response even when the
		//      rule has no EXEC (or noop'd it). User-authored EMIT
		//      entries layer ON TOP of SELECT's synthetic ones so an
		//      explicit EMIT can override a SELECT'd path.
		if op.Resonator.Select != nil {
			synthetic := make([]resonator.BranchValue, 0, len(op.Resonator.Select.Assignments))
			for _, asn := range op.Resonator.Select.Assignments {
				srcPath := normalizeSelectPath(asn.From)
				dstPath := normalizeSelectPath(asn.To)
				val := gjson.Get(op.Input, srcPath)
				useDefault := !val.Exists() ||
					(val.Type == gjson.String && val.String() == "")
				var (
					goVal  interface{}
					rawVal string
				)
				if useDefault && asn.HasDefault {
					resolved, rerr := runtime.Resolve(asn.Default, runtime.JSONEnv(op.Input))
					if rerr != nil {
						pu.Logger.Debug("select default resolve", zap.String("path", asn.To), zap.String("err", rerr.Error()))
						goVal = ""
					} else {
						goVal = resolved
					}
				} else if useDefault {
					goVal = ""
				} else {
					goVal = val.Value()
					rawVal = val.Raw
				}
				// Pre-overlay onto op.Input. SetRaw preserves the
				// source's JSON shape (objects/arrays) when we have
				// the raw bytes; fall back to the Go value for the
				// default-substitution case (which is always a
				// literal we can json-marshal cleanly).
				if rawVal != "" {
					if altered, err := sjson.SetRaw(op.Input, dstPath, rawVal); err == nil {
						op.Input = altered
					}
				} else {
					if altered, err := sjson.Set(op.Input, dstPath, goVal); err == nil {
						op.Input = altered
					}
				}
				synthetic = append(synthetic, resonator.BranchValue{
					Path:  "." + dstPath,
					Value: ast.Literal{V: goVal},
				})
			}
			// Merge into Emit so the writes survive into the
			// post-EXEC overlay. User-authored EMIT (if any) goes
			// last → wins on conflict.
			if op.Resonator.Emit == nil {
				op.Resonator.Emit = &resonator.Set{Overrides: synthetic}
			} else {
				op.Resonator.Emit.Overrides = append(
					append([]resonator.BranchValue{}, synthetic...),
					op.Resonator.Emit.Overrides...,
				)
			}
		}

		ops[j] = op
	}

	// TODO: check Execs to make sure all of same type/one Jump, etc.

	return ops, nil
}

// DecorateInput overrides input with preset values, but only if
// the target path doesn't already exist (set-if-absent semantics —
// used by SET PRE / SET POST).
//
// Each override.Value flows through runtime.Resolve so future Value
// shapes (PathRef, FunctionCall) plumb through without touching
// this function. On a resolution error the call returns the
// partially-updated input plus the error; per the strict-by-default
// semantics in internal docs/todo-txcl-expressions.md §5, callers MUST treat
// the error as a rule-halt signal (silently dropping the SET would
// hide protocol errors).
func (pu *Unit) DecorateInput(input string, overrides []resonator.BranchValue) (string, error) {
	for _, override := range overrides {
		branch := strings.TrimPrefix(override.Path, ".")

		cur := gjson.Get(input, branch)
		if !cur.Exists() {
			val, err := runtime.Resolve(override.Value, runtime.JSONEnv(input))
			if err != nil {
				return input, fmt.Errorf("decorate %s: %w", override.Path, err)
			}
			altered, _ := sjson.Set(input, branch, val) // TODO: should branch be escaped ?
			input = altered
		}
		// exists, no change
	}

	return input, nil
}

// OverlayResponse writes branch values onto a JSON document with
// overwrite semantics — the EMIT clause's runtime. Sibling to
// DecorateInput, which is set-if-absent.
//
// Two distinct documents flow through here:
//   - env: the envelope to RESOLVE PathRef / FunctionCall args against.
//     Typically op.Input — the envelope the op saw at dispatch, so
//     EMIT's PathRefs like `@web.req.body` read live envelope state.
//   - output: the WRITE accumulator. EMIT's resolved values are
//     sjson-set onto this; the merger picks it up after the op
//     returns. Empty/missing becomes "{}" so callers don't need to
//     special-case EMIT-only rules.
//
// Splitting env from output is load-bearing: if both were the same
// document (the EMIT accumulator), `EMIT @x = @env.field` couldn't
// see the envelope at all — `@env.field` would resolve against the
// initially-empty accumulator and come back nil. The two-arg form
// is a fix for that.
//
// Each override.Value flows through runtime.Resolve. Resolution
// errors halt and propagate up (strict-by-default); sjson write
// errors (path unwritable) keep the existing log-and-continue
// behavior because the path syntax is a programming bug rather
// than a value-computation failure.
func (pu *Unit) OverlayResponse(env, output string, overrides []resonator.BranchValue) (string, error) {
	if output == "" {
		output = "{}"
	}
	envForResolve := runtime.JSONEnv(env)
	for _, override := range overrides {
		branch := strings.TrimPrefix(override.Path, ".")
		val, rerr := runtime.Resolve(override.Value, envForResolve)
		if rerr != nil {
			return output, fmt.Errorf("overlay %s: %w", override.Path, rerr)
		}
		altered, err := sjson.Set(output, branch, val)
		if err != nil {
			pu.Logger.Debug("emit overlay", zap.String("path", override.Path), zap.String("err", err.Error()))
			continue
		}
		output = altered
	}
	return output, nil
}

// OpsForStage lookup operations for given stage. If no rows match the requested
// stack, the lookup falls back along stack-prefix boundaries: a request at
// `website/canary/100` that finds nothing tries `website/100`, then “. This
// implements the overlay model — a sparse `website/canary` tree inherits any
// scope it doesn't explicitly override from the parent stack. Wildcard stacks
// (containing SQL LIKE metacharacters like `%`) skip the fallback walk; they
// already match across stacks at one level.
func (pu *Unit) OpsForStage(ctx context.Context, stage string) ([]operation.Operation, error) {
	ctx, span := pu.Mc.Tracer.Start(ctx, `opsforstage`)
	defer span.End()

	ops := make([]operation.Operation, 0)
	stack, scope, err := pu.StageParse(stage)
	if err != nil {
		pu.Logger.Debug("stack error", zap.String("err", err.Error()))
		return ops, err
	}

	pu.Logger.Debug("ops-for-stage", zap.String("stage", stage), zap.String("stack", stack), zap.Int("scope", scope))

	tenant := tenantScope(ctx)
	wildcard := strings.ContainsAny(stack, "%_")
	prefix := stack
	for {
		rows, err := pu.lookupOpsExact(ctx, prefix, scope, tenant)
		if err != nil {
			return ops, err
		}
		if len(rows) > 0 {
			if prefix != stack {
				pu.Logger.Debug("ops-for-stage fallback", zap.String("stack", stack), zap.String("matched", prefix), zap.Int("scope", scope))
			}
			if pu.Logger.Core().Enabled(zap.DebugLevel) {
				if opsJson, err := json.MarshalIndent(rows, "", "\t"); err == nil {
					pu.Logger.Debug("opsforstage result", zap.String("ops", string(opsJson)))
				}
			}
			return rows, nil
		}
		if wildcard {
			return ops, nil
		}
		next, ok := stackParent(prefix)
		if !ok {
			return ops, nil
		}
		prefix = next
	}
}

// tenantPredicate builds the SQL fragment + bind args that scope an
// `ops` query to a tenant. There is NO unfiltered path: a non-empty
// slug resolves to its tenant_id via the tenants table in the same
// opstack snapshot; an empty slug matches only the `tenant_id IS NULL`
// bucket (legacy/test rows that predate tenant attribution). In
// production every request is pinned to a real tenant or to the
// reserved `_sys` tenant on the ingress-miss path, so the IS NULL
// bucket is never hit by live traffic — it only keeps direct
// OpsForStage callers (tests) working. The same fragment is applied to
// the inner MIN(scope) subquery so the scope floor is computed
// within-tenant, not from some other tenant's rows.
func tenantPredicate(tenant string) (string, []any) {
	if tenant == "" {
		return " AND tenant_id IS NULL", nil
	}
	return " AND tenant_id = (SELECT tenant_id FROM tenants WHERE slug = ?)", []any{tenant}
}

// lookupOpsExact runs the underlying SQL against a single stack pattern. The
// scope arg is the requested floor; the inner SELECT picks the minimum scope
// at-or-above it (so authors don't have to fill every scope number).
//
// tenant is the pinned request tenant slug; the lookup is always
// tenant-scoped via tenantPredicate — this is the cross-tenant
// isolation boundary, so a stage jump by bare stack name can only ever
// land in the requesting tenant's stacks.
func (pu *Unit) lookupOpsExact(ctx context.Context, stack string, scope int, tenant string) ([]operation.Operation, error) {
	ops := make([]operation.Operation, 0)

	tenantPred, tenantArgs := tenantPredicate(tenant)
	query := fmt.Sprintf(`SELECT stack, scope, name, txcl, mock_res FROM ops WHERE stack LIKE ?%s AND scope = (SELECT MIN(scope) AS scope FROM ops WHERE scope >= ? AND stack LIKE ?%s);`, tenantPred, tenantPred)
	args := []any{stack}
	args = append(args, tenantArgs...)
	args = append(args, scope, stack)
	args = append(args, tenantArgs...)

	rows, err := pu.opstackDB(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		pu.Logger.Debug("rows error", zap.String("err", err.Error()))
		return ops, err
	}
	defer rows.Close()
	for rows.Next() {
		op := operation.New()
		err = rows.Scan(&op.Stack, &op.Scope, &op.Name, &op.Txcl, &op.MockRes)
		if err != nil {
			pu.Logger.Debug("scan error", zap.String("err", err.Error()))
			return ops, err
		}
		ops = append(ops, *op)
	}
	if err := rows.Err(); err != nil {
		pu.Logger.Debug("rows error", zap.String("err", err.Error()))
		return ops, err
	}
	return ops, nil
}

// stackParent peels the trailing slash-segment off a stack name. Returns the
// parent prefix and ok=true if there's any prefix left. An empty string or a
// stack with no slashes has no parent (ok=false).
func stackParent(stack string) (string, bool) {
	if stack == "" {
		return "", false
	}
	idx := strings.LastIndex(stack, "/")
	if idx < 0 {
		return "", false
	}
	return stack[:idx], true
}

// buildOpstack returns a JSON array describing the operations registered
// for `stack`, grouped by scope: [{"step":100,"ops":["hello","world"]},
// {"step":200,"ops":["sort"]}, ...]. Used to enrich breakpoint responses
// so developers can see what rules exist at each step from the response
// alone, without round-tripping to the admin API. Reads from the per-
// request opstack snapshot so the shape matches what the request saw.
func (pu *Unit) buildOpstack(ctx context.Context, stack string) (string, error) {
	tenantPred, tenantArgs := tenantPredicate(tenantScope(ctx))
	args := append([]any{stack}, tenantArgs...)
	rows, err := pu.opstackDB(ctx).QueryContext(ctx,
		fmt.Sprintf(`SELECT scope, name FROM ops WHERE stack = ?%s ORDER BY scope, name`, tenantPred), args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	type entry struct {
		Step int      `json:"step"`
		Ops  []string `json:"ops"`
	}
	byStep := map[int][]string{}
	var order []int
	for rows.Next() {
		var scope int
		var name string
		if err := rows.Scan(&scope, &name); err != nil {
			return "", err
		}
		if _, seen := byStep[scope]; !seen {
			order = append(order, scope)
		}
		byStep[scope] = append(byStep[scope], name)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	out := make([]entry, 0, len(order))
	for _, s := range order {
		out = append(out, entry{Step: s, Ops: byStep[s]})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
