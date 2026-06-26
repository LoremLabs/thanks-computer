package storeseed

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Scope identifies the stack version a reconcile is materialising. Tenant
// scopes every store call; Stack/Version are carried for logging + future
// prior-version diffing (whole-collection drop, P4).
type Scope struct {
	Tenant  string
	Stack   string
	Version int64
}

// Materializer reconciles the store-seed packs of one Kind into its backing
// store. The concrete materializers live in subpackages (vecseed, kvseed) so
// this core package stays free of store dependencies — the CLI and the
// control-event applier import storeseed only for the path vocabulary.
type Materializer interface {
	// Kind is the pack kind this materializer owns (KindVector | KindKV).
	Kind() string

	// Shared reports whether the backing store is shared across fleet nodes
	// (pgvector, redis) rather than per-node (sqlite-vec, boltdb). A shared
	// store is reconciled exactly ONCE — on the activation origin (the control
	// plane) — so concurrent data-plane appliers don't race the delete-missing
	// pass. A per-node store is reconciled on every node from its CAS-resolved
	// pack. The wiring layer that picks the backend declares this.
	Shared() bool

	// Reconcile makes the store match packs — the full set of this kind's packs
	// in this version. Each pack owns its collection/namespace (managed scope):
	// upsert the pack's items + delete managed items absent from it. A pack with
	// zero items empties (but does not necessarily drop) its target. Whole-
	// collection drop (a pack removed entirely between versions) is the
	// Reconciler's job (P4 prior-version diff), not the Materializer's.
	Reconcile(ctx context.Context, scope Scope, packs []RawPack) error
}

// Reconciler dispatches a version's packs to the registered materializers. It
// is built once at boot (server.go) from the live stores and injected into the
// activation path; reconcile is invoked best-effort AFTER the activation tx
// commits, so a slow or failing store never stalls or rolls back a deploy.
type Reconciler struct {
	byKind map[string]Materializer
}

// NewReconciler builds a Reconciler from the given materializers. A nil or
// empty set yields a no-op Reconciler (Reconcile returns nil) — the open-core
// default when no seedable store is configured. A duplicate Kind is a
// programming error and panics at boot.
func NewReconciler(ms ...Materializer) *Reconciler {
	byKind := make(map[string]Materializer, len(ms))
	for _, m := range ms {
		if m == nil {
			continue
		}
		if _, dup := byKind[m.Kind()]; dup {
			panic(fmt.Sprintf("storeseed: duplicate materializer for kind %q", m.Kind()))
		}
		byKind[m.Kind()] = m
	}
	return &Reconciler{byKind: byKind}
}

// Reconcile groups packs by kind and dispatches each group to its materializer.
// origin marks the activation origin (the control plane): shared-store
// materializers reconcile only when origin is true; per-node materializers
// always run. Best-effort and exhaustive — every materializer is attempted and
// the errors are aggregated, so one failing store doesn't skip the others.
// A pack kind with no registered materializer is reported (a deploy referencing
// a store this node can't seed should be visible, not silent).
func (r *Reconciler) Reconcile(ctx context.Context, scope Scope, packs []RawPack, origin bool) error {
	if r == nil || len(r.byKind) == 0 {
		return nil
	}
	byKind := map[string][]RawPack{}
	for _, p := range packs {
		byKind[p.Kind] = append(byKind[p.Kind], p)
	}

	var errs []error
	for _, kind := range sortedKinds(byKind) {
		m, ok := r.byKind[kind]
		if !ok {
			errs = append(errs, fmt.Errorf("no materializer for pack kind %q (packs: %s)", kind, packNames(byKind[kind])))
			continue
		}
		if m.Shared() && !origin {
			continue // shared store reconciled once, on the origin
		}
		if err := m.Reconcile(ctx, scope, byKind[kind]); err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s: %w", kind, err))
		}
	}
	return errors.Join(errs...)
}

// Kinds returns the pack kinds this Reconciler can materialise (sorted),
// for diagnostics.
func (r *Reconciler) Kinds() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byKind))
	for k := range r.byKind {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKinds(m map[string][]RawPack) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func packNames(packs []RawPack) string {
	names := make([]string, 0, len(packs))
	for _, p := range packs {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
