package openaicompat

import (
	"encoding/json"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

// The wire shapes of the OpenAI Chat Completions API. The neutral
// chat.Message is translated into these before marshalling, so a field the
// dialect does not have (is_error, binding) never reaches a provider, and
// the fields it does have take the provider's form (a tool call's
// arguments are a JSON string here, an object in the neutral shape).
//
// Field order matters: a message without tool fields must marshal to the
// same bytes chat.Message did before this translation existed (role,
// content, name), which the corpus test pins.

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"` // a string, or nil for null
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// wireMessages translates the transcript.
//
//   - An assistant turn whose text is empty and that made calls has
//     `content: null`; every other content is the string as given.
//   - A tool turn carries only role, content and tool_call_id: no name
//     (the API has no such field there) and no error flag (nor has it one;
//     the content says what failed).
//   - A call's arguments go as the JSON text of Arguments, or Raw when the
//     arguments never parsed, or "{}" when a hand-written call has neither.
func wireMessages(msgs []chat.Message) []wireMessage {
	if msgs == nil {
		return nil // marshals as null, as a nil []chat.Message did
	}
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		w := wireMessage{Role: m.Role, Content: m.Content}
		if strings.EqualFold(m.Role, "tool") {
			w.ToolCallID = m.ToolCallID
		} else {
			w.Name = m.Name
		}
		if len(m.ToolCalls) > 0 {
			if m.Content == "" {
				w.Content = nil
			}
			w.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				w.ToolCalls = append(w.ToolCalls, wireToolCall{
					ID:       c.ID,
					Type:     "function",
					Function: wireFunction{Name: c.Name, Arguments: argumentsText(c)},
				})
			}
		}
		out = append(out, w)
	}
	return out
}

func argumentsText(c chat.ToolCall) string {
	if s := strings.TrimSpace(string(c.Arguments)); s != "" && s != "null" {
		return string(c.Arguments)
	}
	if c.Raw != "" {
		return c.Raw
	}
	return "{}"
}

// wireTools translates the offered definitions. Binding is not a field
// here, so it is never sent. A tool with a strict plan is sent with its
// strict schema and `strict: true`; the rest as declared, without the
// field.
func wireTools(tools []chat.Tool, strict map[string]strictPlan) []wireTool {
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		def := wireToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters}
		if p, ok := strict[t.Name]; ok {
			def.Parameters = p.schema
			yes := true
			def.Strict = &yes
		}
		out = append(out, wireTool{Type: "function", Function: def})
	}
	return out
}

// wireToolChoice is "auto", "none" or "required" as a string, and any other
// value as the function object that forces that one tool.
func wireToolChoice(choice string) any {
	switch choice {
	case chat.ToolChoiceAuto, chat.ToolChoiceNone, chat.ToolChoiceRequired:
		return choice
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": choice}}
}

// mapFinish turns the API's finish_reason into the neutral vocabulary. An
// absent reason stays "" (unknown), so a backend that never sends one does
// not look as if it had said "other".
func mapFinish(reason string) string {
	switch reason {
	case "":
		return ""
	case "stop":
		return chat.FinishStop
	case "length":
		return chat.FinishLength
	case "tool_calls", "function_call":
		return chat.FinishToolCalls
	case "content_filter":
		return chat.FinishFiltered
	}
	return chat.FinishOther
}

// parseArguments applies the one rule for a call's arguments: the API's
// argument string that is a JSON object is the arguments; exactly "" (what
// the API sends for a tool without parameters) is {}; anything else is not
// JSON, kept as Raw with Arguments null. Malformed text is never turned
// into {}.
func parseArguments(s string) (args json.RawMessage, raw string) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return json.RawMessage("{}"), ""
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), ""
	}
	return nil, s
}
