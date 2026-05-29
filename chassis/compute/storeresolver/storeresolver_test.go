package storeresolver_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	"github.com/loremlabs/thanks-computer/chassis/artifact/filestore"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/storeresolver"
	_ "github.com/loremlabs/thanks-computer/chassis/compute/wazero" // registers the wazero engine
)

// jsFixture loads the committed Javy/QuickJS module from the wazero package's
// testdata (built with `javy build`); using the real JS artifact proves the
// whole chain end to end, language included.
func jsFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../wazero/testdata/src/js/transform.js.wasm")
	if err != nil {
		t.Fatalf("read JS fixture: %v", err)
	}
	return b
}

func putArtifact(t *testing.T, store artifact.Store, wasm []byte, engine string) compute.Ref {
	t.Helper()
	sum := sha256.Sum256(wasm)
	ref := compute.Ref{Alg: "sha256", Digest: hex.EncodeToString(sum[:])}
	manifest, _ := json.Marshal(storeresolver.Manifest{Engine: engine, Alg: ref.Alg, Digest: ref.Digest})
	if err := store.Put(context.Background(), ref.StoreRef(), wasm, manifest); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	return ref
}

func newStore(t *testing.T) artifact.Store {
	t.Helper()
	s, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore.New: %v", err)
	}
	return s
}

func TestResolveReturnsArtifactWithEngine(t *testing.T) {
	store := newStore(t)
	wasm := jsFixture(t)
	ref := putArtifact(t, store, wasm, "wazero")

	art, err := storeresolver.New(store).Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if art.Engine != "wazero" {
		t.Fatalf("engine = %q, want wazero (from manifest)", art.Engine)
	}
	if len(art.Wasm) != len(wasm) {
		t.Fatalf("wasm len = %d, want %d", len(art.Wasm), len(wasm))
	}
}

func TestResolveNotFound(t *testing.T) {
	store := newStore(t)
	_, err := storeresolver.New(store).Resolve(context.Background(),
		compute.Ref{Alg: "sha256", Digest: "deadbeef"})
	if err != compute.ErrNotFound {
		t.Fatalf("Resolve(absent) err = %v, want compute.ErrNotFound", err)
	}
}

// TestEndToEnd proves the full Phase-2 runtime chain: artifact in the store →
// resolver → Manager → wazero engine → JSON output. Uses the real JS module.
func TestEndToEnd(t *testing.T) {
	store := newStore(t)
	ref := putArtifact(t, store, jsFixture(t), "wazero")

	mgr := compute.NewManager(storeresolver.New(store),
		compute.Limits{MaxMemoryMB: 64, MaxWall: 5 * time.Second})

	out, err := mgr.Run(context.Background(), ref, []byte(`{"in":7}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !gjson.GetBytes(out, "computed").Bool() || gjson.GetBytes(out, "lang").String() != "js" {
		t.Fatalf("unexpected output: %s", out)
	}
	if gjson.GetBytes(out, "in").Int() != 7 {
		t.Fatalf("input not preserved: %s", out)
	}
}
