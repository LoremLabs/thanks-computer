package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
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

// captureTransport answers every request with a canned completion and keeps
// the outbound body, so a test can see what was asked of the provider.
type captureTransport struct{ body []byte }

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.body, _ = io.ReadAll(req.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`)),
		Request:    req,
	}, nil
}

// A schema call whose schema is an object and whose messages mention JSON asks
// for JSON mode; anything else is sent as before, with no response_format.
func TestJSONModeForASchemaCall(t *testing.T) {
	object := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	cases := []struct {
		name   string
		schema json.RawMessage
		system string
		want   bool
	}{
		{"object schema, messages say JSON", object, "Respond with JSON only.", true},
		{"lower-case json counts", object, "answer as json", true},
		{"no schema", nil, "Respond with JSON only.", false},
		{"messages never say JSON", object, "Answer briefly.", false},
		{"array schema", json.RawMessage(`{"type":"array"}`), "Respond with JSON only.", false},
		{"schema without a type", json.RawMessage(`{"properties":{}}`), "Respond with JSON only.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &captureTransport{}
			b, err := chat.Open("openai", chat.Config{HTTPClient: &http.Client{Transport: rt}})
			if err != nil {
				t.Fatalf("open openai: %v", err)
			}
			bag := &secrets.SecretBag{}
			bag.Set("OPENAI_KEY", []byte("sk-test"))
			_, err = b.Run(context.Background(), chat.Request{
				Messages: []chat.Message{{Role: "system", Content: tc.system}, {Role: "user", Content: "hello"}},
				Schema:   tc.schema,
			}, bag)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			var sent struct {
				ResponseFormat *struct {
					Type string `json:"type"`
				} `json:"response_format"`
			}
			if err := json.Unmarshal(rt.body, &sent); err != nil {
				t.Fatalf("outbound body: %v", err)
			}
			got := sent.ResponseFormat != nil && sent.ResponseFormat.Type == "json_object"
			if got != tc.want || (!tc.want && sent.ResponseFormat != nil) {
				t.Errorf("response_format = %+v, want json_object %v (body %s)", sent.ResponseFormat, tc.want, rt.body)
			}
		})
	}
}
