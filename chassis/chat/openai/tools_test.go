package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// The backend states its tool features, and a request with tools reaches
// the wire in the dialect's shape; one without them is unchanged.
func TestToolFeaturesAndWire(t *testing.T) {
	rt := &captureTransport{}
	b, err := chat.Open("openai", chat.Config{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	tc, ok := b.(chat.ToolCapable)
	if !ok {
		t.Fatal("openai backend does not implement ToolCapable")
	}
	if f := tc.ToolFeatures(); !f.Tools || !f.Parallel || !f.Strict {
		t.Errorf("features = %+v", f)
	}
	bag := &secrets.SecretBag{}
	bag.Set("OPENAI_KEY", []byte("sk-test"))
	no := false
	_, err = b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "find it"}},
		Tools: []chat.Tool{{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`),
			Binding: json.RawMessage(`{"capability":"web.search"}`)}},
		ToolChoice: "required",
		Parallel:   &no,
	}, bag)
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(rt.body, &sent); err != nil {
		t.Fatal(err)
	}
	if string(sent["tool_choice"]) != `"required"` || string(sent["parallel_tool_calls"]) != "false" {
		t.Errorf("choice/parallel = %s/%s", sent["tool_choice"], sent["parallel_tool_calls"])
	}
	if string(sent["tools"]) != `[{"type":"function","function":{"name":"web_search","parameters":{"type":"object"}}}]` {
		t.Errorf("tools = %s", sent["tools"])
	}
	if _, has := sent["provider"]; has {
		t.Error("OpenRouter's routing field sent to OpenAI")
	}
}
