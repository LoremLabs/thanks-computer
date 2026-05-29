package trace

import "context"

// ctxKey is unexported so it can't collide with other packages'
// context values.
type ctxKey struct{}

// WithContext returns a copy of parent carrying the given tracer.
// A nil tracer is acceptable — FromContext returns NoopTracer when
// the key is absent, so callers don't need nil checks at every call
// site.
func WithContext(parent context.Context, t RequestTracer) context.Context {
	if t == nil {
		return parent
	}
	return context.WithValue(parent, ctxKey{}, t)
}

// FromContext returns the tracer attached by the bus loop, or a
// NoopTracer if none is set (typical when tests or admin handlers
// drive the processor outside a request lifecycle).
func FromContext(ctx context.Context) RequestTracer {
	if t, ok := ctx.Value(ctxKey{}).(RequestTracer); ok && t != nil {
		return t
	}
	return NoopTracer{}
}
