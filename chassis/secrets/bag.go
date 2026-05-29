package secrets

import (
	"context"
	"sort"
)

// SecretBag is the per-request, in-process-only container for
// materialized secret cleartext. PR 2 of the per-tenant secret store
// (internal docs/todo-secret-store.md §4.1).
//
// The bag is **structurally non-serializable**: every standard
// encoder (json, text, gob) panics if it ever reaches the bag's
// MarshalXxx method. Combined with the `json:"-"` tag on the
// `Operation.Secrets` field, this gives a defense-in-depth invariant:
// cleartext cannot reach any envelope, trace, log, mock, or
// continuation by construction. No taint flag to forget, no
// redaction discipline to maintain — a violation is a panic, not a
// silent leak.
//
// Bag values are cheap to copy: the underlying map is shared by
// reference, so an Operation.Copy() produces a new Operation that
// sees the same materialized secrets. This is intentional — a copied
// op is a new execution instance within the same request scope, and
// must see the same in-process cleartext to do its work.
//
// The zero value is usable: Get returns (nil, false); Set lazy-
// allocates; Names returns nil; Zero is a no-op.
type SecretBag struct {
	entries map[string][]byte
}

// Set stores cleartext under name. Lazy-allocates the underlying map
// on first call. Callers should not retain the slice after Set; the
// bag owns it (and will zero it via Zero()).
func (b *SecretBag) Set(name string, cleartext []byte) {
	if b.entries == nil {
		b.entries = make(map[string][]byte)
	}
	b.entries[name] = cleartext
}

// Get returns the cleartext for name and a presence flag. The
// returned slice aliases bag-owned storage; callers must not mutate.
// After the bag's Zero() runs, the bytes become zero — handlers that
// need to retain the cleartext beyond the bag's lifetime must copy.
func (b SecretBag) Get(name string) (cleartext []byte, ok bool) {
	if b.entries == nil {
		return nil, false
	}
	v, ok := b.entries[name]
	return v, ok
}

// Names returns the set of secret names currently in the bag, sorted
// for deterministic iteration. Useful for audit logging which NAMEs
// an op materialized (NEVER use this to log values).
func (b SecretBag) Names() []string {
	if len(b.entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(b.entries))
	for k := range b.entries {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Zero overwrites every held cleartext with zero bytes and clears
// the map. Safe to call on a zero-value bag and safe to call twice.
//
// The processor (PR 3) calls Zero via `defer` on every exit path
// from Run() — success, error, or panic — so cleartext lives only
// for the duration of one op's execution. Handlers should not call
// Zero themselves; the processor frame owns wipe.
func (b *SecretBag) Zero() {
	if b == nil || b.entries == nil {
		return
	}
	for k, v := range b.entries {
		Zero(v)
		delete(b.entries, k)
	}
}

// Len returns the number of materialized secrets in the bag. Useful
// for tests and for assertions in the processor splice.
func (b SecretBag) Len() int { return len(b.entries) }

// MarshalJSON panics. The `json:"-"` tag on Operation.Secrets keeps
// well-behaved encoders away from the bag; this panic is the
// defense-in-depth guarantee for any code path that tries to marshal
// a bag directly (logger formatters, error wrappers, debug dumpers).
// Failing loud is the point: a silent leak would be worse.
func (b SecretBag) MarshalJSON() ([]byte, error) {
	panic("secrets: SecretBag is not JSON-serializable (cleartext must never leave the process)")
}

// MarshalText panics for the same reason as MarshalJSON. Some
// formatters (e.g. zap's encoder, fmt's %v on structs implementing
// encoding.TextMarshaler) reach for MarshalText before MarshalJSON.
func (b SecretBag) MarshalText() ([]byte, error) {
	panic("secrets: SecretBag is not text-serializable (cleartext must never leave the process)")
}

// GobEncode panics for the same reason. Defends against
// encoding/gob (used by some caching layers and persistence
// shims) reaching the bag.
func (b SecretBag) GobEncode() ([]byte, error) {
	panic("secrets: SecretBag is not gob-serializable (cleartext must never leave the process)")
}

// --- context plumbing for core ops ---
//
// ExecHTTP gets `op` directly and reads op.Secrets. But chassis-core
// ops (`txco://hmac-sign` et al.) are dispatched through the
// OpsHandler interface, which only receives (ctx, opName, in, out)
// — no access to op. To let those handlers consume cleartext, the
// processor's ExecCore wraps ctx with the bag pointer before calling
// the handler. Handlers fish it out via BagFromContext.

type ctxKeyBag struct{}

// WithBag attaches a SecretBag pointer to ctx. The pointer lets
// handlers Get materialized cleartext without copying the bag value
// (which would copy the map header, not the entries — same map
// reference either way, but pointer is the clearer contract).
func WithBag(ctx context.Context, bag *SecretBag) context.Context {
	if bag == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyBag{}, bag)
}

// BagFromContext returns the SecretBag pointer attached to ctx by
// WithBag, or nil if none was attached. A nil return means the
// caller is in a context that didn't materialize secrets (e.g.
// the no-refs fast path), and the handler should treat any secret
// lookup as not-available.
func BagFromContext(ctx context.Context) *SecretBag {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(ctxKeyBag{}).(*SecretBag); ok {
		return v
	}
	return nil
}
