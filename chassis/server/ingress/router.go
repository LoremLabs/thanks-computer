// Package ingress implements the chassis's first-class router: it
// maps source-specific signal keys (HTTP host, TCP listener, cron job)
// to a `(tenant, stack)` target, before any txcl rule evaluation.
//
// The router runs in the server's bus loop, between the inlets'
// envelope construction and `processor.Run`. Hits stamp the envelope
// with `_txc.tenant`/`_txc.stack`/`_txc.ingress` and dispatch into the
// resolved stack; misses fall back to the chassis's default entry
// stage (today `boot/%/0`) without stamping.
//
// The Resolver interface is the seam between this in-process YAML
// implementation and a future DB-backed implementation (chassis-as-
// service). Callers depend only on Resolver; swapping a YAML resolver
// for a DB-backed one is additive.
//
// LMTP routing is a separate concern. Each RCPT TO is resolved
// independently (one delivery, N rcpts, N decisions) and recipients
// that resolve to the same (tenant, stack) get batched into one
// envelope. The MailResolver interface is the add-on that LMTP-aware
// resolvers implement; the chassis tries it before falling back to
// per-envelope Resolve. See `internal docs/todo-lmtp-routing-v2.md`.
package ingress

import (
	"fmt"
	"os"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.yaml.in/yaml/v3"
)

// RouteKey is the bundle of source-specific signals the resolver
// consults when picking a tenant + entry stack for an event.
//
// Each inlet populates a different subset:
//
//	http  → Src, Hostname, Path
//	tcp   → Src, Listener
//	cron  → Src, Job
//
// LMTP does NOT use Resolve(RouteKey) — see MailResolver. Each LMTP
// recipient is its own routing decision; a single RouteKey can't
// model that without losing the per-rcpt independence.
//
// Unused fields stay empty; the resolver only matches on what its
// source provides. Path is captured for HTTP but unused in v1 — it's
// here so callers don't churn when path-prefix matching lands.
type RouteKey struct {
	Src      string
	Hostname string
	Listener string
	Job      string
	Path     string
}

// RouteTarget is the resolver's output: the tenant the event belongs
// to, the stack to enter, the matched key string (for `_txc.ingress`
// observability), and a Verified flag stack rules can gate on.
//
// Verified is true when the routing decision can be trusted to
// reflect proven ownership of the matched key:
//   - YAML matches → true (operator manually configured the route).
//   - DB matches → true iff `tenant_hostnames.verified_at IS NOT NULL`.
//
// In permissive mode, an unverified DB row still produces a match but
// with Verified=false. Stack rules that need stricter behaviour can
// inspect `_txc.hostname_verified` and reject / branch as they see fit.
//
// LMTP Strategy A parses a `+modifier` subaddress from the local-part
// (`tenant.stack+modifier@host`) but does NOT propagate it on
// RouteTarget — the modifier doesn't drive routing, and a "group
// exemplar" stamped on the envelope would be lossy when rcpts in the
// same group carry different modifiers. Rules that want per-rcpt
// modifiers parse `_txc.lmtp.rcpt[i]` directly (split on '+').
type RouteTarget struct {
	Tenant   string
	Stack    string
	Ingress  string
	Verified bool
}

// Resolver maps a RouteKey to a RouteTarget. Returns ok=false when no
// rule matches; callers should fall back to the chassis default entry
// stage and leave the envelope unmodified.
type Resolver interface {
	Resolve(key RouteKey) (RouteTarget, bool)
}

// MailResolver is an add-on interface implemented by resolvers that
// understand LMTP recipient lookups. Each RCPT TO on a single LMTP
// transaction is resolved independently; rcpts that resolve to the
// same (tenant, stack) get batched into one envelope by the LMTP
// inlet.
//
// The YAML resolver implements MailResolver against
// `ingress.lmtp.{recipients,listeners}`. The DB-backed resolver
// (SaaS overlay or open-core's DBResolver wrapper) layers DB lookups
// for verified-domain bypass on top.
//
// `listener` is the LMTP listener name the delivery arrived on
// (currently always "default"; multi-listener configs land later).
// It's used only for the listener-catch-all fallback.
type MailResolver interface {
	ResolveRecipient(rcpt, listener string) (RouteTarget, bool)
}

// File is the on-disk YAML schema v1 loads. It intentionally groups
// routes by source (http/tcp/cron/lmtp) so per-source matching is a
// single map lookup.
type File struct {
	Ingress struct {
		HTTP struct {
			Hosts map[string]Target `yaml:"hosts"`
		} `yaml:"http"`
		TCP struct {
			Listeners map[string]Target `yaml:"listeners"`
		} `yaml:"tcp"`
		Cron struct {
			Jobs map[string]Target `yaml:"jobs"`
		} `yaml:"cron"`
		// LMTP routes are tried in this priority order per RCPT TO:
		//   1. recipients[<exact addr>]   — operator exact override
		//   2. recipients["@" + domain]   — operator domain wildcard
		//   3. Strategy A — tenant.stack[+mod]@<default-host> parse
		//   4. verified_domains[<domain>] — open-core YAML stand-in
		//      for tenant_hostnames lookup (the DB-backed equivalent
		//      lives in DBResolver and queries the same table that
		//      authorizes HTTP routing)
		//   5. listeners[<listener>]      — operator listener catch-all
		// Each recipient resolves independently; the LMTP inlet groups
		// rcpts that resolved to the same (tenant, stack) into one
		// envelope.
		LMTP struct {
			Listeners       map[string]Target `yaml:"listeners"`
			Recipients      map[string]Target `yaml:"recipients"`
			VerifiedDomains map[string]Target `yaml:"verified_domains"`
		} `yaml:"lmtp"`
	} `yaml:"ingress"`
}

// Target is the YAML value side of each per-source map.
type Target struct {
	Tenant string `yaml:"tenant"`
	Stack  string `yaml:"stack"`
}

// yamlResolver is the in-process resolver backed by a parsed File.
type yamlResolver struct {
	file File

	// defaultMailHosts is the set of hosts the chassis answers
	// Strategy A on (`tenant.stack[+mod]@<this host>` → `<tenant>/<stack>`).
	// Configured via WithDefaultMailHosts at load time. Empty disables
	// Strategy A; the resolver falls through to the listener catch-all.
	// Hosts are lowercased for case-insensitive comparison (RFC 5321).
	defaultMailHosts []string
}

// ResolverOption configures a yamlResolver at load time. Use with
// LoadResolverFromFile(path, WithDefaultMailHosts(...)) etc.
type ResolverOption func(*yamlResolver)

// WithDefaultMailHosts configures the hosts on which LMTP Strategy A
// is recognized. An incoming rcpt's host (case-insensitive) must equal
// one of these for the local-part `tenant.stack[+mod]` parse to fire.
//
// Empty hosts in the slice are dropped (defensive against viper's
// CSV-parsing edge cases). Hosts are lowercased so the operator's
// YAML / flag value can be in any case.
func WithDefaultMailHosts(hosts []string) ResolverOption {
	return func(r *yamlResolver) {
		out := make([]string, 0, len(hosts))
		for _, h := range hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h != "" {
				out = append(out, h)
			}
		}
		r.defaultMailHosts = out
	}
}

func (r *yamlResolver) Resolve(key RouteKey) (RouteTarget, bool) {
	// YAML matches are operator-asserted, so Verified=true:
	// the operator manually configured `ingress.http.hosts.<host>`
	// in the YAML file, which is a stronger trust signal than the
	// DNS / HTTP-01 challenge flow.
	switch key.Src {
	case "http":
		if t, ok := r.file.Ingress.HTTP.Hosts[key.Hostname]; ok {
			return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: key.Hostname, Verified: true}, true
		}
	case "tcp":
		if t, ok := r.file.Ingress.TCP.Listeners[key.Listener]; ok {
			return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: key.Listener, Verified: true}, true
		}
	case "cron":
		if t, ok := r.file.Ingress.Cron.Jobs[key.Job]; ok {
			return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: key.Job, Verified: true}, true
		}
	}
	return RouteTarget{}, false
}

// ResolveRecipient implements MailResolver. Each RCPT TO walks this
// priority list (most-specific first); first match wins:
//
//  1. `recipients[<exact addr>]`   — operator-specified exact match
//  2. `recipients["@" + domain]`   — operator-specified domain wildcard
//  3. Strategy A — local-part `tenant.stack[+modifier]` parsed against
//     `defaultMailHosts` (set via WithDefaultMailHosts at load time)
//  4. `verified_domains[<domain>]` — YAML stand-in for the DB-backed
//     tenant_hostnames lookup. DBResolver layers the DB version
//     between this rule and the listener fallback below.
//  5. `listeners[<listener>]`      — operator-specified listener catch-all
//
// The rcpt is lowercased before matching (RFC 5321 §2.3.11: domain
// case-insensitive; local-part conventionally lowercased). YAML keys
// are matched as-written, so operators should write them in
// lowercase too.
func (r *yamlResolver) ResolveRecipient(rcpt, listener string) (RouteTarget, bool) {
	rcpt = strings.ToLower(rcpt)
	if t, ok := r.resolveBeforeListener(rcpt); ok {
		return t, true
	}
	if t, ok := r.resolveListener(listener); ok {
		return t, true
	}
	return RouteTarget{}, false
}

// resolveBeforeListener runs the address-keyed rules (1, 2, 3, 4) —
// everything except the listener catch-all. Exposed for the DB-backed
// resolver (DBResolver) to interleave its own Strategy B between this
// chain and the listener fallback.
//
// The rcpt is expected pre-lowercased by the caller.
func (r *yamlResolver) resolveBeforeListener(rcpt string) (RouteTarget, bool) {
	if t, ok := r.resolveOverrides(rcpt); ok {
		return t, true
	}
	if t, ok := r.resolveStrategyA(rcpt); ok {
		return t, true
	}
	if t, ok := r.resolveVerifiedDomainYAML(rcpt); ok {
		return t, true
	}
	return RouteTarget{}, false
}

// resolveOverrides covers rules 1 + 2 — exact recipient and
// @domain wildcard. Lower-priority rules call this first.
func (r *yamlResolver) resolveOverrides(rcpt string) (RouteTarget, bool) {
	if rcpt == "" {
		return RouteTarget{}, false
	}
	if t, ok := r.file.Ingress.LMTP.Recipients[rcpt]; ok {
		return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: rcpt, Verified: true}, true
	}
	if at := strings.LastIndex(rcpt, "@"); at >= 0 {
		domain := "@" + rcpt[at+1:]
		if t, ok := r.file.Ingress.LMTP.Recipients[domain]; ok {
			return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: domain, Verified: true}, true
		}
	}
	return RouteTarget{}, false
}

// resolveStrategyA covers rule 3 — Strategy A local-part parsing.
// `tenant.stack[+modifier]@<host>` where <host> is in defaultMailHosts.
// Both tenant and stack must be valid slugs (`[a-z][a-z0-9-]*`);
// anything else falls through.
//
// The `+modifier` (RFC 5233 subaddress) is parsed off but NOT carried
// on RouteTarget. Routing keys on (tenant, stack) — `acme.support+monday`
// and `acme.support+tuesday` deliberately route to the same target
// and batch into one envelope. Rules that want the modifier read it
// from `_txc.lmtp.rcpt[i]` and split on '+' — the source of truth.
func (r *yamlResolver) resolveStrategyA(rcpt string) (RouteTarget, bool) {
	if len(r.defaultMailHosts) == 0 || rcpt == "" {
		return RouteTarget{}, false
	}
	at := strings.LastIndex(rcpt, "@")
	if at < 0 {
		return RouteTarget{}, false
	}
	host := rcpt[at+1:]
	if !hostMatchesAny(host, r.defaultMailHosts) {
		return RouteTarget{}, false
	}
	local := rcpt[:at]
	tenant, stack, _, ok := parseStrategyALocal(local)
	if !ok {
		return RouteTarget{}, false
	}
	return RouteTarget{
		Tenant:   tenant,
		Stack:    tenant + "/" + stack,
		Ingress:  "lmtp:" + tenant + "." + stack + "@" + host,
		Verified: true,
	}, true
}

// resolveVerifiedDomainYAML covers rule 4 in the open-core YAML
// resolver — Strategy B's static stand-in for `tenant_hostnames`.
// Operators without a DB can list domains they own as their tenant's
// destination; DBResolver layers the table-backed equivalent on top
// at the same priority slot.
//
// The matched rcpt's domain (post-`@`) is looked up in
// `verified_domains`. A hit routes to:
//   - the entry's `stack:` (if explicitly set), or
//   - `<tenant>/_mail` by convention (the default mail stack)
//
// `Verified: true` is set unconditionally — YAML routes are operator-
// asserted, same as any other YAML entry. The chassis-level
// `_txc.hostname_verified` envelope stamp respects this.
func (r *yamlResolver) resolveVerifiedDomainYAML(rcpt string) (RouteTarget, bool) {
	if len(r.file.Ingress.LMTP.VerifiedDomains) == 0 || rcpt == "" {
		return RouteTarget{}, false
	}
	at := strings.LastIndex(rcpt, "@")
	if at < 0 {
		return RouteTarget{}, false
	}
	domain := rcpt[at+1:]
	t, ok := r.file.Ingress.LMTP.VerifiedDomains[domain]
	if !ok {
		return RouteTarget{}, false
	}
	stack := t.Stack
	if stack == "" {
		stack = t.Tenant + "/_mail"
	}
	return RouteTarget{
		Tenant:   t.Tenant,
		Stack:    stack,
		Ingress:  "domain:" + domain,
		Verified: true,
	}, true
}

// resolveListener covers rule 5 — the listener catch-all.
func (r *yamlResolver) resolveListener(listener string) (RouteTarget, bool) {
	if listener == "" {
		return RouteTarget{}, false
	}
	if t, ok := r.file.Ingress.LMTP.Listeners[listener]; ok {
		return RouteTarget{Tenant: t.Tenant, Stack: t.Stack, Ingress: listener, Verified: true}, true
	}
	return RouteTarget{}, false
}

// hostMatchesAny is the case-insensitive equality check for an rcpt's
// host against the configured default mail hosts. defaultMailHosts is
// pre-lowercased by WithDefaultMailHosts; the rcpt is pre-lowercased
// by ResolveRecipient — so it's a direct string compare here.
func hostMatchesAny(host string, defaultHosts []string) bool {
	for _, h := range defaultHosts {
		if h == host {
			return true
		}
	}
	return false
}

// parseStrategyALocal splits an LMTP local-part into
// (tenant, stack, modifier) per RFC 5233 + the chassis convention:
//
//	tenant.stack             → (tenant, stack, "")
//	tenant.stack+modifier    → (tenant, stack, modifier)
//
// Returns ok=false if either tenant or stack isn't a valid slug
// (`[a-z][a-z0-9-]*`). The caller falls through to the next rule.
//
// The local-part is expected pre-lowercased.
func parseStrategyALocal(local string) (tenant, stack, modifier string, ok bool) {
	if local == "" {
		return "", "", "", false
	}
	// Strip +modifier (RFC 5233). The chassis treats the modifier
	// as opaque input data; no further validation.
	if plus := strings.IndexByte(local, '+'); plus >= 0 {
		modifier = local[plus+1:]
		local = local[:plus]
	}
	// Split on the FIRST dot. tenant/stack slugs don't allow dots,
	// so a second dot is a parse failure (the local-part isn't
	// Strategy A — falls through).
	dot := strings.IndexByte(local, '.')
	if dot < 0 {
		return "", "", "", false
	}
	tenant = local[:dot]
	stack = local[dot+1:]
	if !isSlug(tenant) || !isSlug(stack) {
		return "", "", "", false
	}
	return tenant, stack, modifier, true
}

// isSlug matches `[a-z][a-z0-9-]*` — the chassis's tenant/stack slug
// shape. Used at routing time to reject local-parts that aren't
// Strategy A candidates before any further lookup.
func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
			// ok
		case i > 0 && c >= '0' && c <= '9':
			// digits ok after the first char
		case i > 0 && c == '-':
			// hyphens ok after the first char
		default:
			return false
		}
	}
	return true
}

// LoadResolverFromFile reads path and returns a Resolver. If path is
// empty, returns (nil, nil) — callers interpret nil as "no ingress
// configured" and use the chassis default entry stage. A non-empty
// path that doesn't exist or doesn't parse returns an error so the
// chassis can fail-fast at startup rather than silently route nothing.
//
// Options configure cross-cutting state the YAML file doesn't carry
// (e.g. CLI/env-supplied LMTP default hosts) — see ResolverOption.
func LoadResolverFromFile(path string, opts ...ResolverOption) (Resolver, error) {
	if path == "" {
		// Even without a YAML file, an embedder may want Strategy A
		// (no operator overrides, just convention-based routing). A
		// nil return means "no resolver at all"; an empty
		// yamlResolver with default hosts set is a valid resolver
		// that answers Strategy A only.
		if len(opts) == 0 {
			return nil, nil
		}
		r := &yamlResolver{}
		for _, opt := range opts {
			opt(r)
		}
		if len(r.defaultMailHosts) == 0 {
			return nil, nil
		}
		return r, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ingress config %q: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse ingress config %q: %w", path, err)
	}
	r := &yamlResolver{file: f}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// KeyFromEnvelope extracts the routing signals an inlet stashed on
// the envelope. The field locations are part of the chassis contract:
//
//	_txc.src              → RouteKey.Src
//	_txc.web.req.host     → RouteKey.Hostname   (http; the Host header)
//	_txc.web.req.url.path → RouteKey.Path       (http)
//	_txc.tcp.listener     → RouteKey.Listener   (tcp)
//	_txc.cron.job         → RouteKey.Job        (cron)
//
// LMTP does NOT use this function — its per-RCPT routing happens
// in the LMTP inlet directly against MailResolver.ResolveRecipient,
// and each envelope dispatched by the inlet carries a pre-stamped
// `_txc.route.*` proposal so detectTenantBody no-ops on it.
func KeyFromEnvelope(raw string) RouteKey {
	return RouteKey{
		Src:      gjson.Get(raw, "_txc.src").String(),
		Hostname: gjson.Get(raw, "_txc.web.req.host").String(),
		Listener: gjson.Get(raw, "_txc.tcp.listener").String(),
		Job:      gjson.Get(raw, "_txc.cron.job").String(),
		Path:     gjson.Get(raw, "_txc.web.req.url.path").String(),
	}
}

// StampEnvelope writes the resolver's target onto the envelope as
// _txc.tenant / _txc.stack / _txc.ingress / _txc.hostname_verified.
// The chassis owns these fields — rule authors read them but don't
// write them.
//
// `_txc.hostname_verified` is the load-bearing trust flag: true means
// either the operator hand-configured this route in YAML, or the
// hostname's tenant_hostnames row has verified_at set. False means
// the row is unverified and the chassis is in permissive mode (would
// not have routed in strict mode). Stack rules can gate sensitive
// behaviour on this value without re-reading the DB.
func StampEnvelope(raw string, target RouteTarget) string {
	if raw == "" {
		raw = "{}"
	}
	raw, _ = sjson.Set(raw, "_txc.tenant", target.Tenant)
	raw, _ = sjson.Set(raw, "_txc.stack", target.Stack)
	raw, _ = sjson.Set(raw, "_txc.ingress", target.Ingress)
	raw, _ = sjson.Set(raw, "_txc.hostname_verified", target.Verified)
	return raw
}
