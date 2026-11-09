package openaicompat

import (
	"encoding/json"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

// Strict decoding in this dialect: OpenAI's `strict: true` on a function
// definition and `response_format: {type: json_schema, strict: true}` on a
// schema call. The transform and its undo are the chat package's
// (chat/strict.go); this file decides, per request, which tools and
// whether the answer schema get it, and undoes the nullable wrapping on
// what comes back.

// strictOn reports whether the request asks for strict decoding at all.
func strictOn(req chat.Request) bool {
	return req.Strict == chat.StrictPrefer || req.Strict == chat.StrictRequire
}

// strictTools returns, for a request under strict decoding, the tools whose
// schema qualifies: their strict schema and the wrapped pointers. A tool
// that does not qualify is sent as it is (the handler has already refused
// a `require` request with one).
func strictTools(req chat.Request, o Options) map[string]strictPlan {
	if !strictOn(req) || !o.ToolStrictField {
		return nil
	}
	out := map[string]strictPlan{}
	for _, t := range req.Tools {
		s, wrapped, err := chat.MakeStrict(t.Parameters)
		if err != nil {
			continue
		}
		out[t.Name] = strictPlan{schema: s, wrapped: wrapped}
	}
	return out
}

type strictPlan struct {
	schema  json.RawMessage
	wrapped []string
}

// strictSchema returns the answer schema's strict form when the request is
// under strict decoding, the dialect sends json_schema and the schema
// qualifies; ok is false otherwise and the call falls back to JSON mode.
func strictSchema(req chat.Request, o Options) (plan strictPlan, ok bool) {
	if !strictOn(req) || !o.JSONSchemaFormat || len(req.Schema) == 0 {
		return strictPlan{}, false
	}
	s, wrapped, err := chat.MakeStrict(req.Schema)
	if err != nil {
		return strictPlan{}, false
	}
	return strictPlan{schema: s, wrapped: wrapped}, true
}

// ApplyStrict undoes, on a parsed response, what BuildBody did for strict
// decoding: a null the model returned for a field the chassis made
// nullable is removed from that call's arguments (per tool) and from the
// answer text (under json_schema), so the executor and the schema check
// see the field absent, as the declaration meant. A null at a field the
// declaration itself allowed stays. Backends call it after ParseResponse.
func ApplyStrict(req chat.Request, o Options, resp *chat.Response) {
	plans := strictTools(req, o)
	for i := range resp.ToolCalls {
		c := &resp.ToolCalls[i]
		if p, ok := plans[c.Name]; ok && len(c.Arguments) > 0 {
			c.Arguments = chat.StripWrappedNulls(c.Arguments, p.wrapped)
		}
	}
	if p, ok := strictSchema(req, o); ok && resp.Text != "" {
		resp.Text = string(chat.StripWrappedNulls(json.RawMessage(resp.Text), p.wrapped))
	}
}
