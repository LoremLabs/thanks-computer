package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

func body(t *testing.T, req chat.Request, o Options) map[string]any {
	t.Helper()
	b, err := BuildBody(req, "m", o)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, b)
	}
	return out
}

var searchTool = chat.Tool{
	Name:        "web_search",
	Description: "Search the web.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
	Binding:     json.RawMessage(`{"capability":"web.search"}`),
}

func TestBuildBodyTools(t *testing.T) {
	yes := true
	req := chat.Request{
		Messages:   []chat.Message{{Role: "user", Content: "find it"}},
		Tools:      []chat.Tool{searchTool},
		ToolChoice: chat.ToolChoiceAuto,
		Parallel:   &yes,
	}
	out := body(t, req, Options{})
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", out["tools"])
	}
	tool := tools[0].(map[string]any)
	fn := tool["function"].(map[string]any)
	if tool["type"] != "function" || fn["name"] != "web_search" || fn["description"] != "Search the web." {
		t.Errorf("tool = %v", tool)
	}
	if _, has := fn["binding"]; has {
		t.Error("binding reached the wire")
	}
	if _, has := tool["binding"]; has {
		t.Error("binding reached the wire")
	}
	if _, has := fn["strict"]; has {
		t.Error("strict written without strict decoding")
	}
	if params, _ := fn["parameters"].(map[string]any); params["type"] != "object" {
		t.Errorf("parameters = %v", fn["parameters"])
	}
	if out["tool_choice"] != "auto" || out["parallel_tool_calls"] != true {
		t.Errorf("choice/parallel = %v/%v", out["tool_choice"], out["parallel_tool_calls"])
	}
	if _, has := out["provider"]; has {
		t.Error("provider routing written without RequireParameters")
	}

	// A named choice becomes the function object; none/required stay strings.
	req.ToolChoice = "web_search"
	out = body(t, req, Options{})
	choice, _ := out["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "web_search" {
		t.Errorf("named choice = %v", out["tool_choice"])
	}
	for _, c := range []string{chat.ToolChoiceNone, chat.ToolChoiceRequired} {
		req.ToolChoice = c
		if got := body(t, req, Options{})["tool_choice"]; got != c {
			t.Errorf("choice %s = %v", c, got)
		}
	}

	// Parallel nil is not written; RequireParameters adds the routing field.
	req.Parallel = nil
	out = body(t, req, Options{RequireParameters: true})
	if _, has := out["parallel_tool_calls"]; has {
		t.Error("parallel_tool_calls written when unset")
	}
	if prov, _ := out["provider"].(map[string]any); prov["require_parameters"] != true {
		t.Errorf("provider = %v", out["provider"])
	}
}

func TestBuildBodyRequireParametersOnlyWithTools(t *testing.T) {
	req := chat.Request{Messages: []chat.Message{{Role: "user", Content: "hi"}}, ToolChoice: "auto"}
	out := body(t, req, Options{RequireParameters: true})
	for _, k := range []string{"provider", "tools", "tool_choice", "parallel_tool_calls"} {
		if _, has := out[k]; has {
			t.Errorf("%s written without tools", k)
		}
	}
}

func TestBuildBodyTranscriptRoundTrip(t *testing.T) {
	req := chat.Request{
		Tools: []chat.Tool{searchTool},
		Messages: []chat.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "find it", Name: "ann"},
			{Role: "assistant", Content: "", ToolCalls: []chat.ToolCall{
				{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"tides"}`), Binding: json.RawMessage(`{"capability":"web.search"}`)},
				{ID: "c2", Name: "web_search", Raw: `{"q": "cut`},
				{ID: "c3", Name: "web_search"},
			}},
			{Role: "tool", ToolCallID: "c1", Name: "web_search", Content: `{"results":[]}`, IsError: false},
			{Role: "tool", ToolCallID: "c2", Content: "not json", IsError: true},
			{Role: "tool", ToolCallID: "c3", Content: ""},
			{Role: "assistant", Content: "done", ToolCalls: []chat.ToolCall{{ID: "c4", Name: "web_search", Arguments: json.RawMessage(`{}`)}}},
		},
	}
	b, err := BuildBody(req, "m", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 7 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	// Plain turns are the bytes they always were.
	if string(out.Messages[0]["content"]) != `"be brief"` || string(out.Messages[1]["name"]) != `"ann"` {
		t.Errorf("plain turns changed: %s %s", out.Messages[0], out.Messages[1])
	}
	// The assistant turn with calls and no text has null content and the
	// provider's call shape, arguments as a JSON string.
	a := out.Messages[2]
	if string(a["content"]) != "null" {
		t.Errorf("assistant content = %s, want null", a["content"])
	}
	var calls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
		Binding json.RawMessage `json:"binding"`
	}
	if err := json.Unmarshal(a["tool_calls"], &calls); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || calls[0].Type != "function" || calls[0].Function.Name != "web_search" {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Function.Arguments != `{"q":"tides"}` || calls[1].Function.Arguments != `{"q": "cut` || calls[2].Function.Arguments != `{}` {
		t.Errorf("arguments = %q %q %q", calls[0].Function.Arguments, calls[1].Function.Arguments, calls[2].Function.Arguments)
	}
	if calls[0].Binding != nil {
		t.Error("binding reached the wire on a call")
	}
	// Tool turns carry role, content and tool_call_id only.
	for i := 3; i <= 5; i++ {
		m := out.Messages[i]
		if string(m["role"]) != `"tool"` || string(m["tool_call_id"]) == "" {
			t.Errorf("tool turn %d = %s", i, m)
		}
		for _, k := range []string{"name", "is_error", "tool_calls"} {
			if _, has := m[k]; has {
				t.Errorf("tool turn %d carries %s", i, k)
			}
		}
	}
	if string(out.Messages[3]["content"]) != `"{\"results\":[]}"` || string(out.Messages[5]["content"]) != `""` {
		t.Errorf("tool contents = %s / %s", out.Messages[3]["content"], out.Messages[5]["content"])
	}
	// An assistant turn with text and calls keeps its text.
	if string(out.Messages[6]["content"]) != `"done"` {
		t.Errorf("assistant with text = %s", out.Messages[6]["content"])
	}
}

func TestParseToolCallsAndFinish(t *testing.T) {
	body := `{"id":"chatcmpl-9","model":"gpt-x","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
	  "tool_calls":[
	    {"id":"c1","type":"function","function":{"name":"web_search","arguments":"{\"q\": \"tides\"}"}},
	    {"id":"c2","type":"function","function":{"name":"noop","arguments":""}},
	    {"id":"c3","type":"function","function":{"name":"web_search","arguments":"{\"q\": \"cut"}},
	    {"id":"c4","type":"function","function":{"name":"web_search","arguments":"[1,2]"}},
	    {"id":"c5","type":"function","function":{"name":"web_search","arguments":"null"}}
	  ]}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	resp, err := ParseResponse([]byte(body), "openai", "m", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "" || resp.Finish != chat.FinishToolCalls || resp.ResponseID != "chatcmpl-9" || resp.Model != "gpt-x" {
		t.Errorf("text/finish/id/model = %q/%q/%q/%q", resp.Text, resp.Finish, resp.ResponseID, resp.Model)
	}
	if len(resp.ToolCalls) != 5 {
		t.Fatalf("calls = %d", len(resp.ToolCalls))
	}
	want := []struct{ args, raw string }{
		{`{"q": "tides"}`, ""},
		{`{}`, ""},
		{"", `{"q": "cut`},
		{"", `[1,2]`},
		{"", `null`},
	}
	for i, w := range want {
		c := resp.ToolCalls[i]
		if string(c.Arguments) != w.args || c.Raw != w.raw {
			t.Errorf("call %d: arguments %q raw %q, want %q %q", i, c.Arguments, c.Raw, w.args, w.raw)
		}
		if c.Problem != nil || c.Binding != nil {
			t.Errorf("call %d: the backend set problem/binding", i)
		}
	}
	if resp.ToolCalls[0].ID != "c1" || resp.ToolCalls[0].Name != "web_search" {
		t.Errorf("call 0 = %+v", resp.ToolCalls[0])
	}

	// finish_reason mapping, and a plain answer has no calls.
	for reason, want := range map[string]string{"stop": chat.FinishStop, "length": chat.FinishLength, "tool_calls": chat.FinishToolCalls,
		"function_call": chat.FinishToolCalls, "content_filter": chat.FinishFiltered, "weird": chat.FinishOther, "": ""} {
		fr := `"finish_reason":"` + reason + `",`
		if reason == "" {
			fr = ""
		}
		resp, err := ParseResponse([]byte(`{"choices":[{`+fr+`"message":{"content":"hi"}}]}`), "p", "m", 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if resp.Finish != want || resp.Text != "hi" || len(resp.ToolCalls) != 0 {
			t.Errorf("%q: finish %q text %q calls %d", reason, resp.Finish, resp.Text, len(resp.ToolCalls))
		}
	}
	// A provider that returns the binding key is ignored at the wire.
	resp, err = ParseResponse([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"},"binding":{"capability":"mail.send"}}]}}]}`), "p", "m", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Binding != nil {
		t.Errorf("provider binding reached the call: %+v", resp.ToolCalls)
	}
}

func TestJSONModeIgnoresToolMessages(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	withTool := chat.Request{Schema: schema, Messages: []chat.Message{
		{Role: "user", Content: "answer briefly"},
		{Role: "assistant", ToolCalls: []chat.ToolCall{{ID: "c1", Name: "t", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", ToolCallID: "c1", Content: `{"format":"json"}`},
	}}
	if JSONMode(withTool) {
		t.Error("a tool result mentioning json turned JSON mode on")
	}
	withTool.Messages[0].Content = "answer as JSON"
	if !JSONMode(withTool) {
		t.Error("a user message mentioning JSON did not turn JSON mode on")
	}
}
