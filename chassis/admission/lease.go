package admission

import (
	"context"
	"sync"
)

// Lease is a per-request handle that carries deferred releases — currently
// the concurrency-slot decrement registered by AcquireConcurrency. It is
// created in the server bus-loop goroutine that owns a request's lifetime;
// that goroutine's `defer lease.Release()` runs the registered releases
// exactly once when the request returns (pipeline done OR continuation
// suspend). Nil-safe so the bus loop can defer unconditionally, and Release
// is idempotent (sync.Once) so a stray double-call can't underflow a counter.
//
// Only AcquireConcurrency calls onRelease; only the bus-loop defer calls
// Release. Op / fan-out goroutines carry the lease via context but must
// never Release it.
type Lease struct {
	once sync.Once
	mu   sync.Mutex
	fns  []func()
}

// NewLease returns a fresh, empty lease.
func NewLease() *Lease { return &Lease{} }

// onRelease registers fn to run on Release. Nil-safe.
func (l *Lease) onRelease(fn func()) {
	if l == nil || fn == nil {
		return
	}
	l.mu.Lock()
	l.fns = append(l.fns, fn)
	l.mu.Unlock()
}

// Release runs every registered release function exactly once. Nil-safe and
// idempotent — the bus loop defers it unconditionally, even for requests
// that never acquired a slot (cap==0, denied earlier, or never re-tenanted).
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.mu.Lock()
		fns := l.fns
		l.fns = nil
		l.mu.Unlock()
		for _, fn := range fns {
			fn()
		}
	})
}

// ctxKeyLease is the unexported context key for the per-request lease.
type ctxKeyLease struct{}

// WithLease attaches a lease to ctx for the processor gate to read.
func WithLease(ctx context.Context, l *Lease) context.Context {
	return context.WithValue(ctx, ctxKeyLease{}, l)
}

// LeaseFromContext returns the request's lease, or nil if none is set
// (tests, non-server callers). A nil lease is safe to pass to
// AcquireConcurrency.
func LeaseFromContext(ctx context.Context) *Lease {
	l, _ := ctx.Value(ctxKeyLease{}).(*Lease)
	return l
}
