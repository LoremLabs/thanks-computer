package openai

import (
	"net/http"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	// Also register openrouter so the default-preservation test exercises the
	// real two-backend case (openai linked alongside the v1 default).
	_ "github.com/loremlabs/thanks-computer/chassis/chat/openrouter"
)

func cfg() chat.Config { return chat.Config{HTTPClient: &http.Client{}} }

func TestOpenAIBackendRegistered(t *testing.T) {
	b, err := chat.Open("openai", cfg())
	if err != nil {
		t.Fatalf("open openai: %v", err)
	}
	if b.Name() != "openai" {
		t.Errorf("Name = %q, want openai", b.Name())
	}
	if got := b.RequiredSecrets(); len(got) != 1 || got[0] != "OPENAI_KEY" {
		t.Errorf("RequiredSecrets = %v, want [OPENAI_KEY]", got)
	}
}

func TestProviderOverrideSelectsOpenAI(t *testing.T) {
	b, decision, err := chat.Resolve("openai", cfg())
	if err != nil {
		t.Fatalf("resolve openai: %v", err)
	}
	if b.Name() != "openai" || decision != "provider-override" {
		t.Errorf("resolve openai → %q/%q, want openai/provider-override", b.Name(), decision)
	}
}

// Linking openai must NOT change the provider-less default — it stays openrouter
// (the documented v1 default) regardless of import/registration order.
func TestDefaultStaysOpenRouter(t *testing.T) {
	b, decision, err := chat.Resolve("", cfg())
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if b.Name() != "openrouter" || decision != "default" {
		t.Errorf("default → %q/%q, want openrouter/default", b.Name(), decision)
	}
}
