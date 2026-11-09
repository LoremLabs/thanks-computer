package openaicompat

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

// legacyOpenAIBody is a frozen copy of the body chat/openai/openai.go built
// before this package existed (its lines 83-95 at 1c4946c), and legacyOpenRouterBody
// of chat/openrouter/openrouter.go's (lines 95-98). BuildBody must produce the
// same bytes for every request that has no tools: that is the whole
// backwards-compatibility argument of the extraction.
func legacyOpenAIBody(req chat.Request, model string) []byte {
	outbound := map[string]any{
		"model":    model,
		"messages": req.Messages,
	}
	if legacyJSONMode(req) {
		outbound["response_format"] = map[string]string{"type": "json_object"}
	}
	if req.CacheKey != "" {
		outbound["prompt_cache_key"] = req.CacheKey
	}
	b, _ := json.Marshal(outbound)
	return b
}

func legacyOpenRouterBody(req chat.Request, model string) []byte {
	outbound := map[string]any{
		"model":    model,
		"messages": req.Messages,
	}
	b, _ := json.Marshal(outbound)
	return b
}

func legacyJSONMode(req chat.Request) bool {
	if len(req.Schema) == 0 {
		return false
	}
	var top struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(req.Schema, &top) != nil || top.Type != "object" {
		return false
	}
	for _, m := range req.Messages {
		if strings.Contains(strings.ToLower(m.Content), "json") {
			return true
		}
	}
	return false
}

// corpus is every request shape the backends have sent so far.
var corpus = []struct {
	name string
	req  chat.Request
}{
	{"plain", chat.Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}}},
	{"system and name", chat.Request{Messages: []chat.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi", Name: "ann"}}}},
	{"empty content", chat.Request{Messages: []chat.Message{{Role: "user", Content: ""}}}},
	{"unicode and html", chat.Request{Messages: []chat.Message{{Role: "user", Content: "héllo <b>&</b> 日本"}}}},
	{"schema, json mentioned", chat.Request{Messages: []chat.Message{{Role: "user", Content: "answer as JSON"}}, Schema: json.RawMessage(`{"type":"object"}`)}},
	{"schema, json not mentioned", chat.Request{Messages: []chat.Message{{Role: "user", Content: "answer"}}, Schema: json.RawMessage(`{"type":"object"}`)}},
	{"schema not an object", chat.Request{Messages: []chat.Message{{Role: "user", Content: "json please"}}, Schema: json.RawMessage(`{"type":"array"}`)}},
	{"cache key", chat.Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}, CacheKey: "run_1"}},
	{"schema and cache key", chat.Request{Messages: []chat.Message{{Role: "user", Content: "json"}}, Schema: json.RawMessage(`{"type":"object"}`), CacheKey: "run_2"}},
	{"no messages", chat.Request{}},
	{"model and intent ignored", chat.Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}, Model: "gpt-x", Intent: "label"}},
}

func TestBuildBodyMatchesLegacyMarshal(t *testing.T) {
	for _, tc := range corpus {
		t.Run("openai/"+tc.name, func(t *testing.T) {
			got, err := BuildBody(tc.req, "gpt-4o-mini", Options{JSONMode: true, PromptCacheKey: true})
			if err != nil {
				t.Fatal(err)
			}
			if want := legacyOpenAIBody(tc.req, "gpt-4o-mini"); !bytes.Equal(got, want) {
				t.Errorf("body differs\n got: %s\nwant: %s", got, want)
			}
		})
		t.Run("openrouter/"+tc.name, func(t *testing.T) {
			got, err := BuildBody(tc.req, "openai/gpt-4o-mini", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if want := legacyOpenRouterBody(tc.req, "openai/gpt-4o-mini"); !bytes.Equal(got, want) {
				t.Errorf("body differs\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestParseResponseUsageDetails(t *testing.T) {
	body := `{"model":"gpt-4o-mini-2024","choices":[{"message":{"role":"assistant","content":"hi"}}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":5}}}`
	resp, err := ParseResponse([]byte(body), "openai", "gpt-4o-mini", 7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hi" || resp.Model != "gpt-4o-mini-2024" || resp.Provider != "openai" {
		t.Errorf("text/model/provider = %q/%q/%q", resp.Text, resp.Model, resp.Provider)
	}
	if resp.TokensIn != 100 || resp.TokensOut != 20 || resp.TokensCached != 64 || resp.TokensReasoning != 5 {
		t.Errorf("tokens = %+v", resp)
	}
	if resp.LatencyMS != 7 || resp.Retries != 1 {
		t.Errorf("latency/retries = %d/%d", resp.LatencyMS, resp.Retries)
	}
	// No echo of the model: the caller's name stands.
	resp, err = ParseResponse([]byte(`{"choices":[{"message":{"content":"x"}}]}`), "p", "m", 0, 0)
	if err != nil || resp.Model != "m" || resp.Text != "x" {
		t.Errorf("fallback model: %+v %v", resp, err)
	}
	// Empty and malformed bodies are coded parse errors carrying the partial.
	for _, b := range []string{"", "{not json"} {
		resp, err := ParseResponse([]byte(b), "p", "m", 3, 0)
		if _, ok := err.(*chat.ProviderParseError); !ok {
			t.Errorf("%q: err = %T %v", b, err, err)
		}
		if resp.Provider != "p" || resp.Model != "m" || resp.LatencyMS != 3 {
			t.Errorf("%q: partial = %+v", b, resp)
		}
	}
}

func TestSanitizeBody(t *testing.T) {
	if got := SanitizeBody([]byte(`{"error":"Incorrect API key provided: sk-abc"}`)); strings.Contains(got, "sk-abc") {
		t.Errorf("key leaked: %s", got)
	}
	if got := SanitizeBody([]byte(strings.Repeat("x", 3000))); len(got) != 2048+len("...(truncated)") {
		t.Errorf("not truncated: %d", len(got))
	}
	if got := SanitizeBody([]byte(`{"error":"rate limited"}`)); got != `{"error":"rate limited"}` {
		t.Errorf("plain body changed: %s", got)
	}
}
