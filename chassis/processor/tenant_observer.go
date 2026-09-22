package processor

import (
	"context"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/event"
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
	mu    sync.Mutex
	slug  string
	set   bool
	stack string

	ingress  string
	verified bool

	// acceptedCh is the inlet's Envelope.Accepted channel, if it set
	// one; acceptedSent guards the single send.
	acceptedCh   chan<- event.Acceptance
	acceptedSent bool
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

// observeRoute records the route the boot handoff promoted: the stack it
// routed into, the ingress key that matched and whether the hostname was
// verified. Nil-safe.
func (o *TenantObserver) observeRoute(stack, ingress string, verified bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.stack, o.ingress, o.verified = stack, ingress, verified
	o.mu.Unlock()
}

// Route returns the ingress key and verified flag recorded beside Stack
// at the _sys->tenant handoff; zero values when no handoff happened.
func (o *TenantObserver) Route() (ingress string, hostnameVerified bool) {
	if o == nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.ingress, o.verified
}

// Stack returns the stack recorded at the _sys->tenant handoff (routeBody
// stamps `_txc.stack` from the route proposal; maybeRetenant records it
// here as it rebinds the pin). ok is false when no handoff happened.
func (o *TenantObserver) Stack() (stack string, ok bool) {
	if o == nil {
		return "", false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stack, o.stack != ""
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

// NotifyAccepted registers the channel the processor's acceptance point
// sends on (Envelope.Accepted). A nil channel leaves acceptance silent.
// Nil-safe.
func (o *TenantObserver) NotifyAccepted(ch chan<- event.Acceptance) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.acceptedCh = ch
	o.mu.Unlock()
}

// accepted tells the inlet, once, that the chassis has accepted this
// request for execution in the pinned tenant: the processor calls it
// after the _sys->tenant handoff has been admitted and before the
// tenant's stack runs. The tenant and stack are the observer's own
// immutable record, never the envelope. The send never blocks — the
// inlet buffers its channel — and a second call is a no-op. Nil-safe.
func (o *TenantObserver) accepted() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.acceptedCh == nil || o.acceptedSent {
		return
	}
	o.acceptedSent = true
	select {
	case o.acceptedCh <- event.Acceptance{Tenant: o.slug, Stack: o.stack}:
	default:
	}
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
