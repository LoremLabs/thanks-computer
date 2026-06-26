package storeseed

import (
	"context"
	"errors"
	"testing"
)

type fakeMat struct {
	kind   string
	shared bool
	calls  int
	gotErr error
}

func (f *fakeMat) Kind() string { return f.kind }
func (f *fakeMat) Shared() bool { return f.shared }
func (f *fakeMat) Reconcile(_ context.Context, _ Scope, _ []RawPack) error {
	f.calls++
	return f.gotErr
}

func vpack(name string) RawPack { p, _ := NewRawPack("VECTORS/"+name+".jsonl", nil); return p }
func kpack(name string) RawPack { p, _ := NewRawPack("KV/"+name+".jsonl", nil); return p }

func TestReconcilerNilAndEmpty(t *testing.T) {
	var nilR *Reconciler
	if err := nilR.Reconcile(context.Background(), Scope{}, []RawPack{vpack("x")}, true); err != nil {
		t.Errorf("nil reconciler: %v, want nil", err)
	}
	empty := NewReconciler()
	if err := empty.Reconcile(context.Background(), Scope{}, []RawPack{vpack("x")}, true); err != nil {
		t.Errorf("empty reconciler: %v, want nil", err)
	}
}

func TestReconcilerOriginGatesSharedStore(t *testing.T) {
	ctx := context.Background()
	packs := []RawPack{vpack("books"), kpack("config")}

	// Shared vector store + per-node kv store.
	t.Run("data-plane skips shared", func(t *testing.T) {
		vec := &fakeMat{kind: KindVector, shared: true}
		kv := &fakeMat{kind: KindKV, shared: false}
		r := NewReconciler(vec, kv)
		if err := r.Reconcile(ctx, Scope{}, packs, false /*origin*/); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if vec.calls != 0 {
			t.Errorf("shared vector reconciled on data-plane: calls=%d want 0", vec.calls)
		}
		if kv.calls != 1 {
			t.Errorf("per-node kv not reconciled on data-plane: calls=%d want 1", kv.calls)
		}
	})

	t.Run("origin runs shared", func(t *testing.T) {
		vec := &fakeMat{kind: KindVector, shared: true}
		kv := &fakeMat{kind: KindKV, shared: false}
		r := NewReconciler(vec, kv)
		if err := r.Reconcile(ctx, Scope{}, packs, true /*origin*/); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if vec.calls != 1 || kv.calls != 1 {
			t.Errorf("origin: vec=%d kv=%d want 1,1", vec.calls, kv.calls)
		}
	})
}

func TestReconcilerUnknownKindAndAggregatedErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	vec := &fakeMat{kind: KindVector, gotErr: boom}
	r := NewReconciler(vec)

	// A KV pack with no kv materializer registered → reported, not silent.
	// Plus the vector materializer errors → both surface.
	err := r.Reconcile(ctx, Scope{}, []RawPack{vpack("books"), kpack("config")}, true)
	if err == nil {
		t.Fatal("want aggregated error, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("aggregated error missing the materializer error: %v", err)
	}
	if vec.calls != 1 {
		t.Errorf("vector materializer not called: %d", vec.calls)
	}
}

func TestNewReconcilerDuplicateKindPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("duplicate kind did not panic")
		}
	}()
	NewReconciler(&fakeMat{kind: KindVector}, &fakeMat{kind: KindVector})
}
