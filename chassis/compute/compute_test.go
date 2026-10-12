package compute_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/compute"
	_ "github.com/loremlabs/thanks-computer/chassis/compute/identity"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		alg, dig string
	}{
		{"compute://sha256/abc123", true, "sha256", "abc123"},
		{"compute://blake3/deadbeef", true, "blake3", "deadbeef"},
		{"http://example.com/x", false, "", ""},
		{"compute://sha256/", false, "", ""},     // missing digest
		{"compute://sha256", false, "", ""},      // missing "/digest"
		{"compute:///abc", false, "", ""},        // missing alg
		{"op://name", false, "", ""},             // unresolved op ref, not a compute ref
	}
	for _, c := range cases {
		ref, ok := compute.ParseRef(c.in)
		if ok != c.wantOK {
			t.Errorf("ParseRef(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok && (ref.Alg != c.alg || ref.Digest != c.dig) {
			t.Errorf("ParseRef(%q) = {%q,%q}, want {%q,%q}", c.in, ref.Alg, ref.Digest, c.alg, c.dig)
		}
		if ok && ref.String() != c.in {
			t.Errorf("Ref.String() = %q, want round-trip %q", ref.String(), c.in)
		}
	}
}

func TestOpenEngineUnknown(t *testing.T) {
	if _, err := compute.OpenEngine("no-such-engine", compute.EngineConfig{}); err == nil {
		t.Fatal("OpenEngine(unknown) = nil error, want error listing available engines")
	}
}

func TestOpenEngineIdentityRegistered(t *testing.T) {
	// The blank import above must have self-registered the reference engine.
	if _, err := compute.OpenEngine("identity", compute.EngineConfig{}); err != nil {
		t.Fatalf("identity engine not registered: %v", err)
	}
}

// stubResolver returns a fixed artifact for any ref.
type stubResolver struct {
	art compute.Artifact
	err error
}

func (s stubResolver) Resolve(context.Context, compute.Ref) (compute.Artifact, error) {
	return s.art, s.err
}

func TestManagerRunIdentityEchoes(t *testing.T) {
	m := compute.NewManager(stubResolver{art: compute.Artifact{Engine: "identity"}}, compute.Limits{})
	in := []byte(`{"hello":"world"}`)
	out, err := m.Run(context.Background(), compute.Ref{Alg: "sha256", Digest: "x"}, in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("identity Run = %q, want echo %q", out, in)
	}
}

func TestManagerRunPropagatesResolverError(t *testing.T) {
	m := compute.NewManager(stubResolver{err: compute.ErrNotFound}, compute.Limits{})
	if _, err := m.Run(context.Background(), compute.Ref{Alg: "sha256", Digest: "x"}, []byte(`{}`)); err == nil {
		t.Fatal("Run = nil error, want resolver error propagated")
	}
}

func TestManagerRunUnknownEngine(t *testing.T) {
	m := compute.NewManager(stubResolver{art: compute.Artifact{Engine: "ghost"}}, compute.Limits{})
	if _, err := m.Run(context.Background(), compute.Ref{Alg: "sha256", Digest: "x"}, []byte(`{}`)); err == nil {
		t.Fatal("Run = nil error, want unknown-engine error")
	}
}

func TestManagerRunNoResolver(t *testing.T) {
	m := compute.NewManager(nil, compute.Limits{})
	if _, err := m.Run(context.Background(), compute.Ref{Alg: "sha256", Digest: "x"}, []byte(`{}`)); err == nil {
		t.Fatal("Run with nil resolver = nil error, want error")
	}
}

// countingResolver hands out its artifacts (or err) and counts the calls.
type countingResolver struct {
	arts  map[compute.Ref]compute.Artifact
	err   error
	calls int
}

func (c *countingResolver) Resolve(_ context.Context, ref compute.Ref) (compute.Artifact, error) {
	c.calls++
	if c.err != nil {
		return compute.Artifact{}, c.err
	}
	art, ok := c.arts[ref]
	if !ok {
		return compute.Artifact{}, compute.ErrNotFound
	}
	return art, nil
}

// sha256Ref is the ref the upload endpoint would give these bytes.
func sha256Ref(b []byte) compute.Ref {
	sum := sha256.Sum256(b)
	return compute.Ref{Alg: "sha256", Digest: hex.EncodeToString(sum[:])}
}

// TestManagerResolvesOncePerRef: a ref's artifact is fetched from the store
// once; later runs of it come from memory.
func TestManagerResolvesOncePerRef(t *testing.T) {
	wasm := []byte("module bytes")
	ref := sha256Ref(wasm)
	res := &countingResolver{arts: map[compute.Ref]compute.Artifact{ref: {Alg: ref.Alg, Digest: ref.Digest, Engine: "identity", Wasm: wasm}}}
	m := compute.NewManager(res, compute.Limits{})
	for i := 0; i < 3; i++ {
		if _, err := m.Run(context.Background(), ref, []byte(`{}`)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if res.calls != 1 {
		t.Fatalf("resolver called %d times for one ref, want 1", res.calls)
	}
}

// TestManagerKeepsOnlyMatchingBytes: bytes that don't hash to the ref's
// digest still run (as before) but aren't kept, so a bad read is never reused.
func TestManagerKeepsOnlyMatchingBytes(t *testing.T) {
	ref := sha256Ref([]byte("what was uploaded"))
	res := &countingResolver{arts: map[compute.Ref]compute.Artifact{ref: {Engine: "identity", Wasm: []byte("something else")}}}
	m := compute.NewManager(res, compute.Limits{})
	for i := 0; i < 2; i++ {
		if _, err := m.Run(context.Background(), ref, []byte(`{}`)); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if res.calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (mismatched bytes must not be kept)", res.calls)
	}
}

// TestManagerDoesNotKeepMisses: a ref that wasn't found is looked up again,
// so an artifact uploaded after the miss runs.
func TestManagerDoesNotKeepMisses(t *testing.T) {
	wasm := []byte("late upload")
	ref := sha256Ref(wasm)
	res := &countingResolver{arts: map[compute.Ref]compute.Artifact{}}
	m := compute.NewManager(res, compute.Limits{})
	if _, err := m.Run(context.Background(), ref, []byte(`{}`)); !errors.Is(err, compute.ErrNotFound) {
		t.Fatalf("first run err = %v, want ErrNotFound", err)
	}
	res.arts[ref] = compute.Artifact{Alg: ref.Alg, Digest: ref.Digest, Engine: "identity", Wasm: wasm}
	if _, err := m.Run(context.Background(), ref, []byte(`{}`)); err != nil {
		t.Fatalf("run after upload: %v", err)
	}
	if res.calls != 2 {
		t.Fatalf("resolver called %d times, want 2 (the miss must not be kept)", res.calls)
	}
}
