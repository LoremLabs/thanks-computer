package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

// With tools, OpenRouter is asked to route only to an upstream that takes
// them; without tools the body carries no routing field and no tool fields.
func TestOpenRouterRequireParametersOnlyWithTools(t *testing.T) {
	var bodies [][]byte
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"q\":\"x\"}"}}]}}]}`))
	})
	b := newBackend(t, client)
	if f := b.(chat.ToolCapable).ToolFeatures(); !f.Tools || !f.Parallel || f.Strict {
		t.Errorf("features = %+v", f)
	}
	bag := newBag(t, "or-key")

	resp, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "find it"}},
		Tools:    []chat.Tool{{Name: "web_search", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}, bag)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != chat.FinishToolCalls || len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"q":"x"}` {
		t.Errorf("resp = %+v", resp)
	}
	_, err = b.Run(context.Background(), chat.Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}}, bag)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d", len(bodies))
	}
	var withTools, without map[string]json.RawMessage
	_ = json.Unmarshal(bodies[0], &withTools)
	_ = json.Unmarshal(bodies[1], &without)
	if string(withTools["provider"]) != `{"require_parameters":true}` {
		t.Errorf("provider = %s", withTools["provider"])
	}
	if _, has := withTools["tools"]; !has {
		t.Error("tools not sent")
	}
	for _, k := range []string{"provider", "tools", "tool_choice", "parallel_tool_calls", "prompt_cache_key", "response_format"} {
		if _, has := without[k]; has {
			t.Errorf("%s sent on a plain call", k)
		}
	}
}
