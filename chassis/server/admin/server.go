// Package admin runs the second HTTP server fronting the rule-mutation API.
// It's a sibling to the user-facing web/tcp/cron personalities: separate port,
// separate auth, but shares the same processor + db.
//
// Wire from chassis/server/server.go when "admin" is in --personalities.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/admin/ui"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

type Controller struct {
	ctx      context.Context
	pu       *processor.Unit
	server   *http.Server
	wg       sync.WaitGroup
	registry *registry.Registry
	tenants  *tenants.Store
	nonces   *auth.NonceStore
	verifier signature.Verifier

	// traceReader is the read-side trace backend (file by default; a
	// non-fs backend when admin is split off from the chassis). Built
	// once in Start when the trace routes are mounted.
	traceReader trace.Reader

	// traceArmable is the live-stream backend (registered by stream-
	// capable backends like the NATS overlay; file/noop register none).
	// When present, the admin server mounts GET /traces/stream.
	traceArmable trace.Armable

	// astore is the artifact store used to publish event payloads on
	// mutating admin handlers (fleet-sync producer side). Set by
	// SetArtifactStore from chassis/server/server.go. Nil-safe: when
	// --feed-sink=nop the handlers skip the producer-side work
	// entirely.
	astore artifact.Store

	// unsignedThrottle gates /auth/dev/enroll + /auth/invitations/consume
	// against brute-force probing. Single shared instance so the
	// budget is consistent regardless of which endpoint an attacker
	// alternates against.
	unsignedThrottle *throttle.Throttle

	// devEnrollSecret is the effective dev-enroll secret used by
	// handleDevEnroll. It comes from one of two places:
	//   - an explicit --auth-dev-enroll-secret (or env), in which case
	//     devEnrollAutoGen stays false and the secret behaves exactly
	//     as the operator set it (no burn);
	//   - first-boot auto-generation when the actors table is empty
	//     and no explicit secret is set, in which case
	//     devEnrollAutoGen=true and the secret is single-use (the
	//     handler re-checks HasAnyActiveActor before honouring it).
	// Empty means dev enrollment is disabled.
	devEnrollSecret  string
	devEnrollAutoGen bool

	// oauthIssuer / oauthAudience / oauthJWKS configure POST
	// /auth/oauth/enroll (cloud OIDC-bootstrap enrollment). Empty issuer
	// ⇒ the endpoint is disabled (open-core default: no external issuer
	// is trusted until an operator opts in; the hosted product seeds the
	// canonical issuer at the service layer). oauthJWKS is an
	// auto-refreshing cached key set resolved from the issuer's discovery
	// doc — see resolveOAuthIssuer.
	oauthIssuer   string
	oauthAudience string
	oauthJWKS     jwk.Set
}

func NewController(ctx context.Context, pu *processor.Unit) *Controller {
	return &Controller{ctx: ctx, pu: pu}
}

// SetArtifactStore wires the artifact store the admin handlers use
// to publish event payloads when fleet-sync producer is enabled. The
// chassis boot calls this after opening the artifact store; handlers
// guard internally on FeedSink != nop before touching it.
func (c *Controller) SetArtifactStore(s artifact.Store) { c.astore = s }

func (c *Controller) Start() {
	if !strings.Contains(c.pu.Conf.Personalities, "admin") {
		return
	}

	c.tenants = tenants.New(c.pu.RuntimeDB)
	// Dialect is a pure function of the auth DSN (file: ⇒ SQLite,
	// postgres:// ⇒ shared Postgres for an HA control plane). Derived
	// here rather than threaded through the Unit — it has no state.
	c.registry = registry.NewWithDialect(c.pu.AuthDB, func(ctx context.Context, tenantID string) (string, error) {
		t, err := c.tenants.Lookup(ctx, tenantID)
		if err != nil {
			return "", err
		}
		return t.Slug, nil
	}, registry.DialectForDSN(c.pu.Conf.DbAuthDsn))
	c.nonces = auth.NewNonceStore(10 * time.Minute)
	c.verifier = signature.NewVerifier()
	c.unsignedThrottle = newThrottleFromEnv()

	c.resolveDevEnrollSecret()
	c.resolveOAuthIssuer()

	r := mux.NewRouter()
	// Surface non-2xx admin responses in the chassis log alongside
	// their body. Without this, a 500 from handleCreateDraft (etc.)
	// sends a useful "lookup_stack: database table is locked: stacks"
	// payload to the client but logs nothing server-side — leaving
	// operators to read DevTools to learn what failed. Bypasses
	// static-asset paths and /healthz; see error_logging.go.
	r.Use(c.errorLoggingMiddleware)
	// Unauthenticated probes.
	r.HandleFunc("/healthz", c.handleHealth).Methods(http.MethodGet)
	// Caddy on_demand_tls authorization hook for customer custom
	// domains (loopback-only by deployment; yes/no, no secrets).
	r.HandleFunc(tlsAskPath, c.handleTLSAsk).Methods(http.MethodGet)
	// Admin UI: static SPA, unauthenticated so the browser can load
	// HTML/JS/CSS without signing. The JSON endpoints it calls
	// (/v1/ops, /traces/*) still flow through the auth middleware
	// below — so in signed-mode chassis, the UI loads but its fetches
	// 401 until a browser-friendly auth flow is added.
	r.PathPrefix("/admin/").Handler(ui.Handler("/admin"))
	r.Handle("/admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	// Demo UI: the txcl learning environment, launched by `txco demo`.
	// The standalone /demo/ SPA was merged into admin-ui as the #demo
	// route — redirect any stale bookmark or older `txco demo` shim to
	// its new home. Browsers honor the fragment in Location; CLI clients
	// that strip it land on /admin/ and the SPA's probeDemoMode +
	// syncFromHash route them to #demo. The execution hop endpoints
	// (/v1/demo/fire, /v1/demo/info, /v1/demo/op/build) live on the
	// protected subrouter below — registered under the same demo gate.
	//
	// Gated on --demo-mode (set by `txco demo`): a plain chassis /
	// `txco dev` exposes no demo surface at all, so there is nothing to
	// redirect stale bookmarks to.
	if c.pu.Conf.DemoMode {
		r.Handle("/demo", http.RedirectHandler("/admin/#demo", http.StatusMovedPermanently))
		r.Handle("/demo/", http.RedirectHandler("/admin/#demo", http.StatusMovedPermanently))
	}
	// Bare root is a convenience entry point: send it to the admin UI.
	// 302 (not 301) — this is a UX entry redirect, not a canonical
	// resource move, so it stays uncached if the landing target changes.
	r.Handle("/", http.RedirectHandler("/admin/", http.StatusFound)).Methods(http.MethodGet)
	// Dev enrollment + invitation consume are the chassis's only
	// unsigned credential-mint endpoints. Both share one per-IP
	// throttle so an attacker alternating between them gets one
	// budget, not two. The throttle middleware sits in front of the
	// handler bodies; on disabled-throttle (TXCO_THROTTLE_DISABLED=1
	// or rate=0) it's a pass-through.
	throttled := c.throttleMiddleware(c.unsignedThrottle)
	r.Handle("/auth/dev/enroll",
		throttled(http.HandlerFunc(c.handleDevEnroll))).Methods(http.MethodPost)
	r.Handle("/auth/invitations/consume",
		throttled(http.HandlerFunc(c.handleConsumeInvitation))).Methods(http.MethodPost)
	// Cloud OIDC-bootstrap enrollment — a third unsigned credential-mint
	// sibling, registered only when an issuer is configured (open-core
	// default is disabled; the hosted product seeds the issuer). Shares
	// the same per-IP throttle as the two above.
	if c.oauthIssuer != "" {
		r.Handle("/auth/oauth/enroll",
			throttled(http.HandlerFunc(c.handleOAuthEnroll))).Methods(http.MethodPost)
	}

	// Retired flat routes — return 410 regardless of auth so an
	// operator hitting an old URL in curl immediately sees the
	// migration hint instead of an opaque "invalid_signature". The
	// hint body points at the tenant-scoped replacement and doesn't
	// leak any tenant inventory (the placeholder is "<tenant>", not
	// a real slug). Registered on the top-level mux, BEFORE the
	// protected catch-all subrouter, so they short-circuit first.
	r.HandleFunc("/v1/ops", c.handleRouteRetired).Methods(http.MethodGet)
	// /ops/import was the legacy bulk-upsert path. Retired in favour
	// of the versioned control plane (/stacks/<name>/draft → activate).
	// The flat form returns 410; the tenant-scoped form was never
	// part of the stable surface and simply falls through to 404.
	r.HandleFunc("/v1/ops/import", c.handleRouteRetired).Methods(http.MethodPost)
	r.HandleFunc("/auth/actors", c.handleRouteRetired).Methods(http.MethodGet)
	r.HandleFunc("/auth/actors/{actorID}/revoke", c.handleRouteRetired).Methods(http.MethodPost)
	r.HandleFunc("/auth/invitations", c.handleRouteRetired).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/auth/invitations/{invID}/revoke", c.handleRouteRetired).Methods(http.MethodPost)

	// Browser-friendly auth: when a session cookie is present, the
	// middleware accepts it alongside signed/basic. The registry's
	// session methods plug in here so the middleware doesn't import
	// the full registry type.
	authCfg := auth.Config{
		Mode:           auth.AuthMode(c.pu.Conf.AuthMode),
		BasicUser:      c.pu.Conf.AdminUser,
		BasicPass:      c.pu.Conf.AdminPass,
		Registry:       c.registry,
		Verifier:       c.verifier,
		Nonces:         c.nonces,
		Skew:           5 * time.Minute,
		Sessions:       c.registry,
		AllowedOrigins: c.allowedBrowserOrigins(),
	}

	// /auth/browser/exchange is the one credential-mint endpoint that
	// runs *outside* the auth middleware — the bootstrap token is the
	// proof. Same per-IP throttle as /auth/dev/enroll +
	// /auth/invitations/consume so a single attacker can't probe one
	// endpoint to evade the budget on the others.
	r.Handle("/auth/browser/exchange",
		throttled(http.HandlerFunc(c.handleBrowserExchange))).Methods(http.MethodPost)

	// Everything else goes through the auth middleware.
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		// Wrap each handler in the auth middleware. We have to bridge
		// the gorilla/mux MiddlewareFunc signature to our
		// http.Handler-taking factory.
		return auth.Middleware(authCfg, next)
	})

	// Chassis-wide endpoints stay flat (no tenant prefix). Identity and
	// self-service belong to the principal, not to any one tenant.
	protected.HandleFunc("/auth/whoami", c.handleWhoami).Methods(http.MethodGet)
	protected.HandleFunc("/auth/keys/{keyID}/revoke", c.handleRevokeKey).Methods(http.MethodPost)
	// Browser session endpoints — flat because they operate on the
	// caller's session cookie (which already carries the tenant), not
	// on a tenant resource. Bootstrap lives in the tenant subrouter
	// below where its tenant scope is explicit in the URL.
	protected.HandleFunc("/auth/browser/session", c.handleBrowserSession).Methods(http.MethodGet)
	protected.HandleFunc("/auth/browser/session", c.handleBrowserSessionDelete).Methods(http.MethodDelete)
	// Tenant directory: listing is visible to anyone with at least one
	// membership; creation is super_admin only. Both handlers
	// authenticate themselves; the path is flat because the tenant is
	// either being listed or being created — neither is "scoped to a
	// tenant" in the way ops or invitations are.
	protected.HandleFunc("/v1/tenants", c.handleListTenants).Methods(http.MethodGet)
	protected.HandleFunc("/v1/tenants", c.handleCreateTenant).Methods(http.MethodPost)
	// Fleet reconcile: re-emit current control-plane state as fleet-sync
	// events so lagging replicas converge. super_admin only (chassis-wide);
	// non-destructive (upserts + stack.activated, never deletes).
	protected.HandleFunc("/v1/fleet/resync", c.handleFleetResync).Methods(http.MethodPost)
	// Demo execution-hop endpoints — registered only under --demo-mode
	// (set by `txco demo`). Their presence is the signal the admin-ui's
	// probeDemoMode uses to auto-route to #demo, so a plain chassis /
	// `txco dev` must NOT register them: probeDemoMode then 404s and the
	// UI lands on the normal admin interface.
	if c.pu.Conf.DemoMode {
		// Demo execution hop: proxy a synthetic request to this chassis's
		// own (loopback) web inlet and return the result + rid. Not
		// tenant-scoped — it's a dev/local convenience for the /demo/ UI.
		// Goes through the auth middleware like the rest of /v1 (open in
		// dev).
		protected.HandleFunc("/v1/demo/fire", c.handleDemoFire).Methods(http.MethodPost)
		protected.HandleFunc("/v1/demo/info", c.handleDemoInfo).Methods(http.MethodGet)
		// Demo walkthrough curriculum: the source of truth for the
		// tracks/steps/ops the admin-ui's #demo route renders. Single
		// data structure lives in chassis/demo (Go); the SPA fetches it
		// here on mount rather than embedding a duplicate copy. The
		// same data is used by `txco demo`'s pre-seed (chassis/cli/demo
		// → demo.Seed), so what the SPA shows always matches what was
		// actually seeded into this chassis.
		protected.HandleFunc("/v1/demo/curriculum", c.handleDemoCurriculum).Methods(http.MethodGet)
		// Demo compute-op build hop: bundle + compile a single JS/TS source
		// via the same toolchain `txco apply` uses, then store the wasm
		// artifact in this chassis's astore so the runtime resolver finds it
		// on EXEC. Returns the `compute://sha256/…` ref the client
		// substitutes into the op's txcl.
		protected.HandleFunc("/v1/demo/op/build", c.handleDemoBuildOp).Methods(http.MethodPost)
	}

	// Chassis-global DNS synthesis config (nameservers / edge IPs / MX
	// host that parameterize every delegated zone's synthesized
	// pattern). Deployment infrastructure, not per-tenant data, so it
	// lives here rather than under /v1/tenants/{t}. See
	// internal docs/todo-dns-authority.md.
	protected.HandleFunc("/v1/dns/config", c.handleGetDNSConfig).Methods(http.MethodGet)
	protected.HandleFunc("/v1/dns/config", c.handlePutDNSConfig).Methods(http.MethodPut)

	// Tenant-scoped subrouter. The resolveTenantMiddleware extracts
	// {tenant}, looks up the row, and stamps slug + id onto
	// auth.Context. Handlers are the same Go funcs as the flat
	// versions above; phase 3 makes their capability checks consult
	// the tenant.
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/ops", c.handleListOps).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/actors", c.handleListActors).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/actors/{actorID}/revoke", c.handleRevokeActor).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/members", c.handleListTenantMembers).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/members", c.handleGrantMember).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/members/{actorID}", c.handleRevokeMember).Methods(http.MethodDelete)
	tenantR.HandleFunc("/auth/invitations", c.handleListInvitations).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/invitations", c.handleCreateInvitation).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/invitations/{invID}/revoke", c.handleRevokeInvitation).Methods(http.MethodPost)
	// Browser-auth mint + session admin. Bootstrap is tenant-scoped so
	// the URL is the natural place for the tenant binding; the
	// resulting cookie carries the tenant from then on.
	tenantR.HandleFunc("/auth/browser/bootstrap", c.handleBrowserBootstrap).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/sessions", c.handleListBrowserSessions).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/sessions/{sessionID}", c.handleRevokeBrowserSession).Methods(http.MethodDelete)

	// Versioned opstack control plane. `ops` remains the runtime read
	// model; these endpoints manage the canonical stacks + versions +
	// files behind it. Activation is the only writer to `ops` in this
	// flow.
	tenantR.HandleFunc("/stacks", c.handleListStacks).Methods(http.MethodGet)
	tenantR.HandleFunc("/stacks/{name:.+}/draft", c.handleCreateDraft).Methods(http.MethodPost)
	tenantR.HandleFunc("/stacks/{name:.+}/activate", c.handleActivateStack).Methods(http.MethodPost)
	tenantR.HandleFunc("/stacks/{name:.+}/diff", c.handleDiffVersions).Methods(http.MethodGet)
	tenantR.HandleFunc("/stacks/{name:.+}/versions", c.handleListVersions).Methods(http.MethodGet)
	tenantR.HandleFunc("/stacks/{name:.+}/versions/{n:[0-9]+}/files", c.handlePutDraftFiles).Methods(http.MethodPut)
	tenantR.HandleFunc("/stacks/{name:.+}/versions/{n:[0-9]+}/files", c.handlePatchDraftFile).Methods(http.MethodPatch)
	tenantR.HandleFunc("/stacks/{name:.+}/versions/{n:[0-9]+}/files", c.handleDeleteDraftFile).Methods(http.MethodDelete)
	tenantR.HandleFunc("/stacks/{name:.+}/versions/{n:[0-9]+}/validate", c.handleValidateVersion).Methods(http.MethodPost)
	tenantR.HandleFunc("/stacks/{name:.+}/versions/{n:[0-9]+}", c.handleGetVersion).Methods(http.MethodGet)
	tenantR.HandleFunc("/stacks/{name:.+}", c.handleGetStack).Methods(http.MethodGet)

	// Content-addressed compute artifacts (op:// computes). Upload is
	// tenant-authenticated (opstack caps); storage is global + content-
	// addressed (the digest IS the identity), so PUT is idempotent. `txco
	// apply` HEADs then PUTs the wasm before activating a rule that
	// references compute://<alg>/<digest>.
	tenantR.HandleFunc("/computes/{alg}/{digest}", c.handlePutCompute).Methods(http.MethodPut)
	tenantR.HandleFunc("/computes/{alg}/{digest}", c.handleHeadCompute).Methods(http.MethodHead)

	// Hostname → tenant routing. Each row binds `Host: foo.local` to
	// a (tenant, stack) for the data-plane router; the ingress DB
	// resolver reads them from the dbcache mirror on every HTTP
	// request that misses the YAML map.
	tenantR.HandleFunc("/hostnames", c.handleListHostnames).Methods(http.MethodGet)
	tenantR.HandleFunc("/hostnames", c.handleCreateHostname).Methods(http.MethodPost)
	tenantR.HandleFunc("/hostnames/{hostname}", c.handleRevokeHostname).Methods(http.MethodDelete)
	tenantR.HandleFunc("/hostnames/{hostname}/attach", c.handleAttachHostname).Methods(http.MethodPost)
	tenantR.HandleFunc("/hostnames/{hostname}/challenges", c.handleCreateChallenge).Methods(http.MethodPost)
	tenantR.HandleFunc("/hostnames/{hostname}/verify", c.handleVerifyHostname).Methods(http.MethodPost)
	// Read-only state-of-the-hostname (incl. current verify token without
	// rotating it). Closes the rotation footgun: pre-status, the only
	// way to "see" the token was `challenge`, which rotated it.
	tenantR.HandleFunc("/hostnames/{hostname}/status", c.handleHostnameStatus).Methods(http.MethodGet)

	// Operator runtime-state controls (super_admin): suspend a tenant so its
	// requests are denied (deny_status, default 402) before its stack runs,
	// or resume it. The admission provider picks up the change on the dbcache
	// reload the handler triggers. The billing system drives the same
	// tenant_runtime_state table via entitlement.updated events.
	tenantR.HandleFunc("/suspend", c.handleSuspendTenant).Methods(http.MethodPost)
	tenantR.HandleFunc("/resume", c.handleResumeTenant).Methods(http.MethodPost)
	tenantR.HandleFunc("/limits", c.handleSetTenantLimits).Methods(http.MethodPost)

	// Authoritative-DNS zone preview (read-only). Renders the zone(s)
	// this tenant would be served, in zone-file form — the same
	// snapshot the dns head answers from. `?zone=<origin>` filters to
	// one zone. See internal docs/todo-dns-authority.md §6.7.
	tenantR.HandleFunc("/dns/render", c.handleDNSRender).Methods(http.MethodGet)

	// Delegated-zone + override-record CRUD (no SQL). A pattern zone is
	// synthesized by the dns head; manual zones / override records are
	// the materialized layer. See internal docs/todo-dns-authority.md.
	tenantR.HandleFunc("/dns/zones", c.handleListZones).Methods(http.MethodGet)
	tenantR.HandleFunc("/dns/zones", c.handleCreateZone).Methods(http.MethodPost)
	tenantR.HandleFunc("/dns/zones/{origin}", c.handleRevokeZone).Methods(http.MethodDelete)
	tenantR.HandleFunc("/dns/zones/{origin}/records", c.handleListRecords).Methods(http.MethodGet)
	tenantR.HandleFunc("/dns/zones/{origin}/records", c.handleCreateRecord).Methods(http.MethodPost)
	tenantR.HandleFunc("/dns/zones/{origin}/records", c.handleRevokeRecord).Methods(http.MethodDelete)

	// Per-tenant secret store CRUD. Reveal-never is enforced by
	// response shape: only POST /generate and POST /{name}/rotate-
	// generated emit a `value` field. See internal docs/todo-secret-store.md
	// §5-§6 and chassis/server/admin/secret_endpoints.go.
	tenantR.HandleFunc("/secrets", c.handleListSecrets).Methods(http.MethodGet)
	tenantR.HandleFunc("/secrets", c.handleCreateSecret).Methods(http.MethodPost)
	tenantR.HandleFunc("/secrets/generate", c.handleGenerateSecret).Methods(http.MethodPost)
	tenantR.HandleFunc("/secrets/{name}", c.handleShowSecret).Methods(http.MethodGet)
	tenantR.HandleFunc("/secrets/{name}", c.handleUpdateSecretDescription).Methods(http.MethodPatch)
	tenantR.HandleFunc("/secrets/{name}", c.handleRevokeSecret).Methods(http.MethodDelete)
	tenantR.HandleFunc("/secrets/{name}/rotate", c.handleRotateSecret).Methods(http.MethodPost)
	tenantR.HandleFunc("/secrets/{name}/rotate-generated", c.handleRotateSecretGenerated).Methods(http.MethodPost)

	// Browse the trace dir written by chassis/trace's FileSink.
	// Mounted only when trace-mode != off — otherwise the path returns
	// 404s, and we don't want to mislead by registering a working
	// directory listing that's always empty. http.FileServer provides
	// directory listing for free.
	if c.pu.Conf.TraceMode != "" && c.pu.Conf.TraceMode != "off" && c.pu.Conf.TraceDir != "" {
		// Live-stream of closed traces — backs the admin UI "live"
		// mode. Mounted INDEPENDENTLY of the Reader: a backend may
		// supply an Armable without a Reader (e.g. NATS body-on-bus
		// where archive is a separate R2-backed Reader; or a dev setup
		// that's stream-only). /traces/stream must be registered before
		// any catch-all PathPrefix("/traces/") below — gorilla/mux
		// matches in declaration order.
		if armable, aerr := trace.OpenArmable(c.pu.Conf.TraceStore, trace.StoreConfig{
			Dir:  c.pu.Conf.TraceDir,
			Mode: trace.ParseMode(c.pu.Conf.TraceMode),
		}); aerr == nil {
			c.traceArmable = armable
			// Flat = super-admin chassis-wide; tenant-scoped confines a
			// tenant-owner to their own traces. Same handler — it branches on
			// the resolved tenant in auth.Context (see traceTenantScope).
			protected.HandleFunc("/traces/stream", c.handleTraceStream).Methods(http.MethodGet)
			tenantR.HandleFunc("/traces/stream", c.handleTraceStream).Methods(http.MethodGet)
			c.pu.Logger.Info("traces stream endpoint mounted",
				zap.String("store", c.pu.Conf.TraceStore),
				zap.String("path", "/traces/stream"))
		} else {
			c.pu.Logger.Debug("no live-stream Armable for this trace store; /traces/stream not mounted",
				zap.String("store", c.pu.Conf.TraceStore),
				zap.String("err", aerr.Error()))
		}

		// Read backend selected by --trace-store (file by default; a
		// non-fs backend when admin is a separate machine). The handlers
		// go through this Reader, not the filesystem, so the JSON/ETag/
		// grep contract is identical regardless of where traces live.
		tr, terr := trace.OpenReader(c.pu.Conf.TraceStore, trace.StoreConfig{
			Dir:  c.pu.Conf.TraceDir,
			Mode: trace.ParseMode(c.pu.Conf.TraceMode),
		})
		if terr != nil {
			c.pu.Logger.Error("trace reader open failed; archive endpoints disabled (live /traces/stream remains available if the backend supplied an Armable)",
				zap.String("store", c.pu.Conf.TraceStore),
				zap.String("err", terr.Error()))
		} else {
			c.traceReader = tr
			// Flat routes are super-admin / operator (chassis-wide, incl.
			// _sys); the tenant-scoped copies confine a tenant-owner to their
			// own traces. Same handler funcs — they branch on the resolved
			// tenant in auth.Context (see traceTenantScope). The HTML index +
			// raw browse stay flat-only (operator file-backend conveniences).
			// Must be registered BEFORE the catch-all PathPrefix below —
			// gorilla/mux matches in declaration order.
			protected.HandleFunc("/traces/requests/", c.handleTraceRequestsIndex).Methods(http.MethodGet)
			// JSON list of recent traces — backs `txco trace` (no rid).
			protected.HandleFunc("/traces/requests.json", c.handleTraceList).Methods(http.MethodGet)
			// JSON aggregator for a single request — backs `txco trace <rid>`.
			protected.HandleFunc("/traces/requests/{rid}.json", c.handleTraceRequest).Methods(http.MethodGet)
			// Tenant-scoped copies (per-tenant isolation).
			tenantR.HandleFunc("/traces/requests.json", c.handleTraceList).Methods(http.MethodGet)
			tenantR.HandleFunc("/traces/requests/{rid}.json", c.handleTraceRequest).Methods(http.MethodGet)
			// Raw artifact browse: only when the backend is
			// filesystem-shaped; non-fs backends 404 the raw path.
			if fsys, ok := tr.RawFS(); ok {
				// Raw artifact browse is chassis-wide (it serves any tenant's
				// trace files); gate it behind super-admin like the flat HTML
				// index. traceTenantScope returns ("", super-admin) on this
				// non-{tenant} path.
				rawFS := http.StripPrefix("/traces/", http.FileServer(fsys))
				protected.PathPrefix("/traces/").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, okScope := c.traceTenantScope(w, r); !okScope {
						return
					}
					rawFS.ServeHTTP(w, r)
				})
			} else {
				protected.PathPrefix("/traces/").HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "raw trace browse unavailable for this backend", http.StatusNotFound)
				})
			}
			c.pu.Logger.Info("traces endpoint mounted",
				zap.String("store", c.pu.Conf.TraceStore),
				zap.String("dir", c.pu.Conf.TraceDir),
				zap.String("path", "/traces/"))
		}
	}

	c.server = &http.Server{
		Addr:           c.pu.Conf.AdminAddr,
		Handler:        r,
		ReadTimeout:    time.Duration(c.pu.Conf.AdminReadTimeout) * time.Second,
		WriteTimeout:   time.Duration(c.pu.Conf.AdminWriteTimeout) * time.Second,
		IdleTimeout:    time.Duration(c.pu.Conf.AdminIdleTimeout) * time.Second,
		MaxHeaderBytes: 1 << 18,
	}

	// Startup WARNs:
	//  - legacy open-dev mode (no basic-auth and not signed-only),
	//  - dev enrollment status (operator-supplied OR auto-generated).
	if c.pu.Conf.AdminUser == "" && c.pu.Conf.AuthMode != string(auth.ModeSigned) {
		c.pu.Logger.Warn("admin server: no basic-auth configured (dev only)",
			zap.String("addr", c.pu.Conf.AdminAddr))
	}
	c.logDevEnrollBanner()

	// Pre-bind synchronously so a port conflict surfaces with a clear,
	// actionable error BEFORE the chassis logs "admin controller
	// started" and BEFORE the chassis appears ready. The previous
	// version used Logger.Error inside the goroutine, which silently
	// logged the failure but let the rest of the chassis run — a
	// stale chassis bound to :8081 could shadow a fresh one and the
	// operator would see no admin endpoint with no obvious cause.
	listener, err := net.Listen("tcp", c.pu.Conf.AdminAddr)
	if err != nil {
		c.pu.Logger.Fatal("admin port already in use (or otherwise unbindable)",
			zap.String("addr", c.pu.Conf.AdminAddr),
			zap.String("err", err.Error()),
			zap.String("hint", fmt.Sprintf("lsof -iTCP%s -sTCP:LISTEN", c.pu.Conf.AdminAddr)))
	}

	go func() {
		c.pu.Logger.Info("admin controller started",
			zap.String("port", c.pu.Conf.AdminAddr),
			zap.String("auth_mode", c.pu.Conf.AuthMode))
		c.wg.Add(1)
		defer c.wg.Done()
		if err := c.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			// At this point the listener was bound successfully — any
			// error here is a genuine Serve fault, not a missing port,
			// so fail loud rather than letting the chassis run blind.
			c.pu.Logger.Fatal("admin server", zap.String("err", err.Error()))
		}
		c.pu.Logger.Info("admin shutdown")
	}()
}

func (c *Controller) Stop() {
	if !strings.Contains(c.pu.Conf.Personalities, "admin") {
		return
	}
	if c.server == nil {
		return
	}
	c.pu.Logger.Info("calling admin controller stop")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.server.Shutdown(ctx); err != nil {
		c.pu.Logger.Error("admin server shutdown", zap.String("err", err.Error()))
	}
	c.wg.Wait()
}

// resolveDevEnrollSecret decides whether the admin server should
// accept /auth/dev/enroll calls during this boot, and with what
// secret. Three outcomes:
//
//	explicit secret set       → use as-is, autoGen=false (no burn).
//	registry has any actor    → leave empty, no log.   (already bootstrapped)
//	neither                   → generate a 4-word secret, autoGen=true,
//	                            burn-after-use enforced by handleDevEnroll.
//
// Runs once at Start (after registry init, before handler mount); the
// effective state is read at request time from c.devEnrollSecret /
// c.devEnrollAutoGen.
func (c *Controller) resolveDevEnrollSecret() {
	if c.pu.Conf.AuthDevEnrollSecret != "" {
		c.devEnrollSecret = c.pu.Conf.AuthDevEnrollSecret
		c.devEnrollAutoGen = false
		return
	}
	has, err := c.registry.HasAnyActiveActor(c.ctx)
	if err != nil {
		// On query failure, behave as if the registry is non-empty so
		// we don't accidentally expose a fresh bootstrap path on a
		// chassis whose registry is just temporarily unreachable.
		c.pu.Logger.Error("admin: HasAnyActiveActor failed; auto-bootstrap disabled",
			zap.String("err", err.Error()))
		return
	}
	if has {
		return
	}
	secret, err := auth.EightWordSecret()
	if err != nil {
		c.pu.Logger.Error("admin: EightWordSecret failed; auto-bootstrap disabled",
			zap.String("err", err.Error()))
		return
	}
	c.devEnrollSecret = secret
	c.devEnrollAutoGen = true
}

// logDevEnrollBanner prints the appropriate startup banner for the
// dev-enrollment state computed by resolveDevEnrollSecret. The
// auto-generated branch is the noisy one — operators need the secret
// value in a copy-pasteable form. The explicit branch keeps the
// historical WARN without exposing the secret.
func (c *Controller) logDevEnrollBanner() {
	switch {
	case c.devEnrollAutoGen:
		c.pu.Logger.Warn(
			"first-boot bootstrap autogenerated",
			zap.String("secret", c.devEnrollSecret),
			zap.String("cmd", fmt.Sprintf(
				`txco auth bootstrap-local --secret %q --url %s`,
				c.devEnrollSecret, c.bootstrapHintURL())))
	case c.devEnrollSecret != "":
		c.pu.Logger.Warn(
			"DEV ENROLLMENT ENABLED — POST /auth/dev/enroll grants admin:all to anyone with the shared secret. NEVER set --auth-dev-enroll-secret in production.",
			zap.String("env", c.pu.Conf.Environment))
	}
}

// bootstrapHintURL is a best-effort guess at the URL an operator should
// pass to `txco auth bootstrap-local --url`, so the logged command is
// copy-pasteable. The chassis binds :8081 behind a proxy and can't know
// its true external URL, but in a public deploy the admin UI's Origin
// allowlist (--admin-cors-origins) IS the public admin URL, so prefer
// its first entry. Otherwise fall back to the CLI's own default
// (http://localhost<admin-addr>) — exactly right for local/dev.
func (c *Controller) bootstrapHintURL() string {
	for _, o := range c.pu.Conf.AdminCorsOrigins {
		if o = strings.TrimSpace(o); o != "" {
			return o
		}
	}
	addr := c.pu.Conf.AdminAddr
	if addr == "" {
		addr = ":8081"
	}
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}

// handleHealth is the unauthenticated liveness probe. By default it returns
// the legacy plain-text "ok\n" so load-balancer / uptime probes are
// unchanged. When the caller asks for JSON (Accept: application/json or
// ?format=json) it returns the server's build identity plus the operator-
// configured client-version policy — the txco CLI reads this for self-sync,
// and it doubles as a "what's deployed?" debug surface. Status stays 200.
func (c *Controller) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !healthWantsJSON(r) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	b := c.pu.Conf.Build
	type clientPolicy struct {
		Latest           string `json:"latest,omitempty"`
		MinimumSupported string `json:"minimum_supported,omitempty"`
		Critical         bool   `json:"critical,omitempty"`
	}
	resp := struct {
		Status         string        `json:"status"`
		Version        string        `json:"version,omitempty"`
		Commit         string        `json:"commit,omitempty"`
		Chassis        string        `json:"chassis,omitempty"`
		BuildTimestamp string        `json:"build_timestamp,omitempty"`
		Client         *clientPolicy `json:"client,omitempty"`
	}{
		Status:         "ok",
		Version:        b.Version,
		Commit:         b.Commit,
		Chassis:        b.Chassis,
		BuildTimestamp: b.BuildTimestamp,
	}
	// Only advertise a policy block when the operator set at least one value;
	// a vanilla chassis stays silent (the CLI treats absence as "no policy").
	if c.pu.Conf.ClientVersionLatest != "" || c.pu.Conf.ClientVersionMinimum != "" || c.pu.Conf.ClientVersionCritical {
		resp.Client = &clientPolicy{
			Latest:           c.pu.Conf.ClientVersionLatest,
			MinimumSupported: c.pu.Conf.ClientVersionMinimum,
			Critical:         c.pu.Conf.ClientVersionCritical,
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// healthWantsJSON reports whether /healthz should return the rich JSON form
// rather than the plain-text probe response: an explicit ?format=json, or an
// Accept header that prefers application/json.
func healthWantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("format") == "json" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// authedUser extracts the actor id (signed) or basic-auth user.
// Stored in op_revisions.applied_by. Empty when no auth was used.
func authedUser(r *http.Request) string {
	if ctx := auth.FromContext(r.Context()); ctx != nil {
		switch ctx.Source {
		case "signed":
			return ctx.ActorID
		case "basic":
			user, _, ok := r.BasicAuth()
			if ok {
				return user
			}
		}
	}
	return ""
}
