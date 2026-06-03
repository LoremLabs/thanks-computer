package processor

import (
	"context"
	"sync"
)

// TenantObserver records the tenant a request is pinned to, so the server can
// attribute usage/billing from immutable pipeline state rather than the mutable
// response envelope. The envelope's `_txc.tenant` is display/debug/routing
// metadata an author-controlled stack can rewrite; billing must not trust it.
//
// It mirrors the admission.Lease holder pattern: created in the server bus-loop
// goroutine that owns a request's lifetime, attached to the request context,
// and read back after the pipeline returns. The processor's tenant-pin sites
// (first pin in Run; the one-way `_sys`->concrete retenant) record the resolved
// slug via WithTenant. Last write wins — for a routed request that is the
// concrete tenant; for an unrouted one it stays `_sys`.
//
// Nil-safe so non-server callers (tests, CLI) can ignore it.
type TenantObserver struct {
	mu   sync.Mutex
	slug string
	set  bool
}

// NewTenantObserver returns a fresh observer with no tenant recorded yet.
func NewTenantObserver() *TenantObserver { return &TenantObserver{} }

// observe records the resolved tenant slug. Nil-safe.
func (o *TenantObserver) observe(slug string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.slug = slug
	o.set = true
	o.mu.Unlock()
}

// Tenant returns the last-recorded tenant slug and whether any pin was
// observed. ok is false if the pipeline never pinned a tenant (e.g. a request
// that errored before Run pinned), letting the caller fall back.
func (o *TenantObserver) Tenant() (slug string, ok bool) {
	if o == nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.slug, o.set
}

// ctxKeyTenantObserver is the unexported context key for the per-request
// tenant observer.
type ctxKeyTenantObserver struct{}

// WithTenantObserver attaches a tenant observer to ctx for the processor's pin
// sites to record into.
func WithTenantObserver(ctx context.Context, o *TenantObserver) context.Context {
	return context.WithValue(ctx, ctxKeyTenantObserver{}, o)
}

// tenantObserverFromContext returns the request's tenant observer, or nil.
func tenantObserverFromContext(ctx context.Context) *TenantObserver {
	o, _ := ctx.Value(ctxKeyTenantObserver{}).(*TenantObserver)
	return o
}
