package decide

import (
	"context"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

type stubBackend struct{ name string }

func (s stubBackend) Name() string              { return s.name }
func (s stubBackend) DefaultModel() string      { return "stub-model" }
func (s stubBackend) RequiredSecrets() []string { return nil }
func (s stubBackend) Decide(context.Context, Request, *secrets.SecretBag) (Response, error) {
	return Response{Provider: s.name}, nil
}

func TestResolveDefaultOverrideUnknown(t *testing.T) {
	resetForTests()
	t.Cleanup(resetForTests)
	Register("alpha", func(Config) (Backend, error) { return stubBackend{"alpha"}, nil })
	Register("beta", func(Config) (Backend, error) { return stubBackend{"beta"}, nil })

	b, routing, err := Resolve("", Config{})
	if err != nil || b.Name() != "alpha" || routing != "default" {
		t.Fatalf("default resolve: b=%v routing=%q err=%v", b, routing, err)
	}
	b, routing, err = Resolve("beta", Config{})
	if err != nil || b.Name() != "beta" || routing != "provider-override" {
		t.Fatalf("override resolve: b=%v routing=%q err=%v", b, routing, err)
	}
	if _, _, err := Resolve("gamma", Config{}); err == nil {
		t.Fatal("unknown provider: want error")
	} else if e, ok := err.(*NoBackendError); !ok || e.Code() != "txco_decide_no_backend" {
		t.Fatalf("unknown provider err=%T %v, want *NoBackendError", err, err)
	}

	resetForTests()
	if _, _, err := Resolve("", Config{}); err == nil {
		t.Fatal("empty registry: want error")
	}
}
