package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageUnmarshalContentShapes(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"string", `{"role":"user","content":"hi"}`, "hi", false},
		{"null", `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","name":"t","arguments":{}}]}`, "", false},
		{"absent", `{"role":"assistant"}`, "", false},
		{"object on tool", `{"role":"tool","tool_call_id":"c1","content":{"a": [1, 2]}}`, `{"a":[1,2]}`, false},
		{"array on tool", `{"role":"tool","tool_call_id":"c1","content":[1, "x"]}`, `[1,"x"]`, false},
		{"number on tool", `{"role":"tool","tool_call_id":"c1","content":42}`, `42`, false},
		{"array on user", `{"role":"user","content":[{"type":"text","text":"hi"}]}`, "", true},
		{"object on assistant", `{"role":"assistant","content":{"x":1}}`, "", true},
		{"bad json", `{"role":"user","content":"hi"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Message
			err := json.Unmarshal([]byte(tc.in), &m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && m.Content != tc.want {
				t.Errorf("content = %q, want %q", m.Content, tc.want)
			}
		})
	}
	// A slice decodes element by element through the same method, and the
	// new fields decode with their tags.
	var msgs []Message
	if err := json.Unmarshal([]byte(`[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","name":"t","arguments":{"q":1}}]},{"role":"tool","tool_call_id":"c1","is_error":true,"content":{"ok":false}}]`), &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].ID != "c1" || string(msgs[0].ToolCalls[0].Arguments) != `{"q":1}` {
		t.Errorf("assistant turn = %+v", msgs[0])
	}
	if msgs[1].ToolCallID != "c1" || !msgs[1].IsError || msgs[1].Content != `{"ok":false}` {
		t.Errorf("tool turn = %+v", msgs[1])
	}
	// Marshalling an ordinary message is unchanged: no new keys appear.
	b, _ := json.Marshal(Message{Role: "user", Content: "hi", Name: "ann"})
	if string(b) != `{"role":"user","content":"hi","name":"ann"}` {
		t.Errorf("marshal = %s", b)
	}
}

func TestValidateTools(t *testing.T) {
	good := Tool{Name: "web_search", Description: "search", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`), Binding: json.RawMessage(`{"capability":"web.search"}`)}
	if err := ValidateTools([]Tool{good}); err != nil {
		t.Fatalf("good: %v", err)
	}
	cases := []struct {
		name string
		tool Tool
		kind string
		want string
	}{
		{"dotted name", Tool{Name: "web.search", Parameters: good.Parameters}, KindToolInvalid, "name"},
		{"empty name", Tool{Parameters: good.Parameters}, KindToolInvalid, "name"},
		{"long name", Tool{Name: strings.Repeat("a", 65), Parameters: good.Parameters}, KindToolInvalid, "name"},
		{"no parameters", Tool{Name: "t"}, KindToolInvalid, "parameters"},
		{"parameters not object", Tool{Name: "t", Parameters: json.RawMessage(`["x"]`)}, KindToolInvalid, "JSON object"},
		{"parameters not object schema", Tool{Name: "t", Parameters: json.RawMessage(`{"type":"string"}`)}, KindToolInvalid, `"type": "object"`},
		{"parameters do not compile", Tool{Name: "t", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"nope"}}}`)}, KindToolInvalid, "parameters:"},
		{"remote ref", Tool{Name: "t", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"$ref":"file:///etc/hosts"}}}`)}, KindToolInvalid, "refused"},
		{"binding not object", Tool{Name: "t", Parameters: good.Parameters, Binding: json.RawMessage(`"web.search"`)}, KindToolInvalid, "binding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTools([]Tool{tc.tool})
			te, ok := err.(*ToolRequestError)
			if !ok {
				t.Fatalf("err = %T %v, want *ToolRequestError", err, err)
			}
			if te.Kind != tc.kind || !strings.Contains(te.Reason, tc.want) {
				t.Errorf("err = %v, want kind %s mentioning %q", err, tc.kind, tc.want)
			}
			if te.Code() != "txco_chat_"+tc.kind {
				t.Errorf("code = %s", te.Code())
			}
		})
	}
	dup := good
	dup.Description = "another"
	err := ValidateTools([]Tool{good, dup})
	if te, ok := err.(*ToolRequestError); !ok || te.Kind != KindToolNames {
		t.Errorf("duplicate: err = %v", err)
	}
	if err := ValidateTools(nil); err != nil {
		t.Errorf("none: %v", err)
	}
}

func TestToolRequestErrorShape(t *testing.T) {
	e := &ToolRequestError{Kind: KindPendingToolResult, Reason: "2 calls have no result", IDs: []string{"a", "b"}}
	if e.Code() != CodePendingToolResult || !strings.Contains(e.Error(), "pending_tool_result") {
		t.Errorf("code/error = %s / %s", e.Code(), e.Error())
	}
	var ce CodedError = e
	if ce.Code() != CodePendingToolResult {
		t.Error("not a CodedError")
	}
	var de DetailedError = e
	if d := de.ErrorDetail(); d == nil || len(d["ids"].([]string)) != 2 {
		t.Errorf("detail = %v", d)
	}
	if (&ToolRequestError{Kind: KindToolInvalid, Reason: "x"}).ErrorDetail() != nil {
		t.Error("detail without ids should be nil")
	}
	if !strings.Contains(e.AsJSON(), `"code":"txco_chat_pending_tool_result"`) {
		t.Errorf("AsJSON = %s", e.AsJSON())
	}
}
