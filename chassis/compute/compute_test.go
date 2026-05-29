package compute_test

import (
	"context"
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
