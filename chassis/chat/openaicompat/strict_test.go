package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

var optionalTool = chat.Tool{
	Name:       "web_search",
	Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"count":{"type":"integer"}},"required":["q"]}`),
}

var untypedTool = chat.Tool{
	Name:       "loose",
	Parameters: json.RawMessage(`{"type":"object","properties":{"anything":{}}}`),
}

func TestStrictToolsWire(t *testing.T) {
	strictOpts := Options{ToolStrictField: true, JSONSchemaFormat: true}
	// Off: schemas as declared, no strict field.
	out := body(t, chat.Request{Messages: []chat.Message{{Role: "user", Content: "x"}}, Tools: []chat.Tool{optionalTool}}, strictOpts)
	fn := out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, has := fn["strict"]; has {
		t.Error("strict written with strict off")
	}
	if req := fn["parameters"].(map[string]any)["required"].([]any); len(req) != 1 {
		t.Errorf("schema transformed with strict off: %v", fn["parameters"])
	}
	// Prefer: the qualifying tool is strict with the wrapped schema; the
	// untyped one goes as declared, without the field.
	out = body(t, chat.Request{Messages: []chat.Message{{Role: "user", Content: "x"}}, Tools: []chat.Tool{optionalTool, untypedTool}, Strict: chat.StrictPrefer}, strictOpts)
	tools := out["tools"].([]any)
	ws := tools[0].(map[string]any)["function"].(map[string]any)
	if ws["strict"] != true {
		t.Errorf("web_search not strict: %v", ws)
	}
	params := ws["parameters"].(map[string]any)
	if len(params["required"].([]any)) != 2 || params["additionalProperties"] != false {
		t.Errorf("strict schema = %v", params)
	}
	if _, wrapped := params["properties"].(map[string]any)["count"].(map[string]any)["anyOf"]; !wrapped {
		t.Errorf("optional count not wrapped: %v", params)
	}
	loose := tools[1].(map[string]any)["function"].(map[string]any)
	if _, has := loose["strict"]; has {
		t.Error("non-qualifying tool marked strict")
	}
	// A backend without the strict field never writes it, prefer or not.
	out = body(t, chat.Request{Messages: []chat.Message{{Role: "user", Content: "x"}}, Tools: []chat.Tool{optionalTool}, Strict: chat.StrictPrefer}, Options{})
	fn = out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, has := fn["strict"]; has {
		t.Error("strict written by a backend without the field")
	}
}

func TestStrictSchemaResponseFormat(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"},"note":{"type":"string"}},"required":["answer"]}`)
	msgs := []chat.Message{{Role: "user", Content: "answer as JSON"}}
	// Without strict: today's json_object rule, byte for byte the corpus's shape.
	out := body(t, chat.Request{Messages: msgs, Schema: schema}, Options{JSONMode: true, JSONSchemaFormat: true})
	if rf := out["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format without strict = %v", rf)
	}
	if SchemaMode(chat.Request{Messages: msgs, Schema: schema}, Options{JSONMode: true, JSONSchemaFormat: true}) != "json_object" {
		t.Error("schema mode without strict")
	}
	// With strict and a qualifying schema: json_schema, strict, wrapped.
	req := chat.Request{Messages: msgs, Schema: schema, Strict: chat.StrictPrefer}
	out = body(t, req, Options{JSONMode: true, JSONSchemaFormat: true})
	rf := out["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true || js["name"] != "answer" {
		t.Errorf("response_format = %v", rf)
	}
	if sch := js["schema"].(map[string]any); len(sch["required"].([]any)) != 2 {
		t.Errorf("strict answer schema = %v", sch)
	}
	if SchemaMode(req, Options{JSONMode: true, JSONSchemaFormat: true}) != "json_schema" {
		t.Error("schema mode with strict")
	}
	// A non-qualifying schema under prefer falls back to json_object; a
	// backend without JSONSchemaFormat never sends json_schema.
	loose := chat.Request{Messages: msgs, Schema: json.RawMessage(`{"type":"object","properties":{"x":{}}}`), Strict: chat.StrictPrefer}
	if rf := body(t, loose, Options{JSONMode: true, JSONSchemaFormat: true})["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("fallback = %v", rf)
	}
	if _, has := body(t, req, Options{})["response_format"]; has {
		t.Error("response_format written by a backend without JSON mode")
	}
	if SchemaMode(req, Options{}) != "" {
		t.Error("schema mode on a backend that sent nothing")
	}
}

func TestApplyStrictStripsWrappedNulls(t *testing.T) {
	opts := Options{JSONMode: true, JSONSchemaFormat: true, ToolStrictField: true}
	req := chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "json"}},
		Tools:    []chat.Tool{optionalTool, untypedTool},
		Schema:   json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"},"note":{"type":"string"},"n":{"type":["integer","null"]}},"required":["answer","n"]}`),
		Strict:   chat.StrictPrefer,
	}
	resp := chat.Response{
		Text: `{"answer":"a","note":null,"n":null}`,
		ToolCalls: []chat.ToolCall{
			{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"x","count":null}`)},
			{ID: "c2", Name: "loose", Arguments: json.RawMessage(`{"anything":null}`)},
			{ID: "c3", Name: "web_search", Raw: "not json"},
		},
	}
	ApplyStrict(req, opts, &resp)
	if string(resp.ToolCalls[0].Arguments) != `{"q":"x"}` {
		t.Errorf("wrapped null kept: %s", resp.ToolCalls[0].Arguments)
	}
	if string(resp.ToolCalls[1].Arguments) != `{"anything":null}` {
		t.Errorf("non-strict tool's null removed: %s", resp.ToolCalls[1].Arguments)
	}
	if resp.ToolCalls[2].Raw != "not json" || resp.ToolCalls[2].Arguments != nil {
		t.Errorf("raw call touched: %+v", resp.ToolCalls[2])
	}
	// The answer: the wrapped optional note goes, the declared-nullable n stays.
	if resp.Text != `{"answer":"a","n":null}` {
		t.Errorf("text = %s", resp.Text)
	}
	// Off: nothing is touched.
	req.Strict = ""
	resp.Text = `{"answer":"a","note":null}`
	resp.ToolCalls[0].Arguments = json.RawMessage(`{"q":"x","count":null}`)
	ApplyStrict(req, opts, &resp)
	if resp.Text != `{"answer":"a","note":null}` || string(resp.ToolCalls[0].Arguments) != `{"q":"x","count":null}` {
		t.Error("strict off changed the response")
	}
}
