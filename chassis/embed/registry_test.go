package embed

import (
	"context"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

type stubBackend struct{ name string }

func (s stubBackend) Name() string              { return s.name }
func (s stubBackend) Capabilities() []string    { return nil }
func (s stubBackend) DefaultModel() string      { return "stub-model" }
func (s stubBackend) RequiredSecrets() []string { return nil }
func (s stubBackend) Embed(context.Context, Request, *secrets.SecretBag) (Response, error) {
	return Response{Vectors: [][]float32{{1}}, Provider: s.name, Dimensions: 1}, nil
}

func TestResolveDefaultOverrideUnknown(t *testing.T) {
	resetForTests()
	Register("alpha", func(Config) (Backend, error) { return stubBackend{"alpha"}, nil })
	Register("beta", func(Config) (Backend, error) { return stubBackend{"beta"}, nil })

	// default = first-registered = alpha
	b, routing, err := Resolve("", Config{})
	if err != nil || b.Name() != "alpha" || routing != "default" {
		t.Fatalf("default resolve: b=%v routing=%q err=%v", b, routing, err)
	}

	// explicit override
	b, routing, err = Resolve("beta", Config{})
	if err != nil || b.Name() != "beta" || routing != "provider-override" {
		t.Fatalf("override resolve: b=%v routing=%q err=%v", b, routing, err)
	}

	// unknown provider
	if _, _, err := Resolve("gamma", Config{}); err == nil {
		t.Fatal("unknown provider: want error")
	} else if _, ok := err.(*NoBackendError); !ok {
		t.Fatalf("unknown provider err=%T, want *NoBackendError", err)
	}

	// no backends registered
	resetForTests()
	if _, _, err := Resolve("", Config{}); err == nil {
		t.Fatal("empty registry: want error")
	}
}
