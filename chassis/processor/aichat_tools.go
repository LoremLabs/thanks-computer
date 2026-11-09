package processor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// The tool half of ai://chat: what the handler does around a request that
// offers tools or carries tool calls. It checks the transport shape of the
// transcript and of the answer, never the arguments against a schema (the
// executor's, with txco://schema/check) and never which tool may run (the
// gateway's, when the executor calls it).

// chatRepairErrorResults is the one value WITH repair takes: answer every
// unanswered call with a synthetic error result. For importing a historical
// transcript, never for resuming a live run, whose unanswered calls the
// executor must settle itself.
const chatRepairErrorResults = "error_results"

// checkTranscript walks the messages as the provider will and enforces the
// rules every tool-calling API shares:
//
//   - the ids of one assistant turn's calls are unique;
//   - a tool message answers exactly one outstanding call, in any order;
//   - every outstanding call is answered before any other message, and
//     before the transcript ends.
//
// Breaking the first two is the author's mistake (invalid): a transcript
// the provider would refuse as malformed. Breaking the third is a state the
// executor has to resolve (pending): calls were made whose results are not
// in the transcript yet. With repair = "error_results" those calls get a
// synthetic error result where their results should have been, and the
// repaired transcript is returned.
func checkTranscript(msgs []chat.Message, repair string) (out []chat.Message, pending, invalid *chat.ToolRequestError) {
	out = make([]chat.Message, 0, len(msgs))
	outstanding := map[string]bool{}
	var order []string // outstanding ids, in call order

	settle := func() {
		for _, id := range order {
			out = append(out, chat.Message{Role: "tool", ToolCallID: id, Content: "no result was recorded", IsError: true})
		}
		outstanding = map[string]bool{}
		order = nil
	}
	pendingErr := func() *chat.ToolRequestError {
		ids := append([]string(nil), order...)
		return &chat.ToolRequestError{
			Kind:   chat.KindPendingToolResult,
			Reason: fmt.Sprintf("%d tool call(s) have no result in the transcript: %s", len(ids), strings.Join(ids, ", ")),
			IDs:    ids,
		}
	}

	for i, m := range msgs {
		switch strings.ToLower(m.Role) {
		case "tool":
			if m.ToolCallID == "" {
				return nil, nil, &chat.ToolRequestError{Kind: chat.KindToolInvalid, Reason: fmt.Sprintf("messages[%d]: a tool message needs tool_call_id", i)}
			}
			if !outstanding[m.ToolCallID] {
				return nil, nil, &chat.ToolRequestError{Kind: chat.KindToolInvalid, Reason: fmt.Sprintf("messages[%d]: tool message answers %q, which is not an outstanding call (never made, or answered already)", i, m.ToolCallID)}
			}
			delete(outstanding, m.ToolCallID)
			kept := order[:0]
			for _, id := range order {
				if id != m.ToolCallID {
					kept = append(kept, id)
				}
			}
			order = kept
		case "assistant":
			if len(order) > 0 {
				if repair == chatRepairErrorResults {
					settle()
				} else {
					return nil, pendingErr(), nil
				}
			}
			seen := map[string]bool{}
			for j, c := range m.ToolCalls {
				if c.ID == "" {
					return nil, nil, &chat.ToolRequestError{Kind: chat.KindToolInvalid, Reason: fmt.Sprintf("messages[%d].tool_calls[%d]: a tool call needs an id", i, j)}
				}
				if seen[c.ID] {
					return nil, nil, &chat.ToolRequestError{Kind: chat.KindToolInvalid, Reason: fmt.Sprintf("messages[%d].tool_calls[%d]: id %q repeats within one turn", i, j, c.ID)}
				}
				seen[c.ID] = true
				outstanding[c.ID] = true
				order = append(order, c.ID)
			}
		default:
			if len(order) > 0 {
				if repair == chatRepairErrorResults {
					settle()
				} else {
					return nil, pendingErr(), nil
				}
			}
		}
		out = append(out, m)
	}
	if len(order) > 0 {
		if repair == chatRepairErrorResults {
			settle()
		} else {
			return nil, pendingErr(), nil
		}
	}
	return out, nil, nil
}

// gateToolFeatures refuses, before any secret is materialized or anything
// is sent, a request that asks a backend for what it does not guarantee: a
// backend without tools, parallel control it lacks, or strict decoding it
// lacks when the request requires it. Translate what you can guarantee;
// refuse what you cannot.
func gateToolFeatures(backend chat.Backend, w chatWith) *chat.ToolRequestError {
	var f chat.ToolFeatures
	if tc, ok := backend.(chat.ToolCapable); ok {
		f = tc.ToolFeatures()
	}
	if len(w.tools) > 0 {
		if !f.Tools {
			return &chat.ToolRequestError{Kind: chat.KindToolsUnsupported, Reason: fmt.Sprintf("backend %q takes no tools", backend.Name())}
		}
		if w.parallel != nil && !f.Parallel {
			return &chat.ToolRequestError{Kind: chat.KindToolsUnsupported, Reason: fmt.Sprintf("backend %q cannot bound parallel tool calls (WITH parallel)", backend.Name())}
		}
	}
	if w.strict == chat.StrictRequire && (len(w.tools) > 0 || len(w.schema) > 0) {
		if !f.Strict {
			return &chat.ToolRequestError{Kind: chat.KindStrictUnsupported, Reason: fmt.Sprintf("backend %q does not decode strictly, and WITH strict = \"require\" asks for it", backend.Name())}
		}
		// Required strict decoding needs every schema to qualify: a
		// `prefer` would fall back to ordinary decoding; `require` refuses.
		for _, t := range w.tools {
			if ok, why := chat.Qualifies(t.Parameters); !ok {
				return &chat.ToolRequestError{Kind: chat.KindStrictUnsupported, Reason: fmt.Sprintf("tool %q cannot be decoded strictly (%s), and WITH strict = \"require\" asks for it", t.Name, why)}
			}
		}
		if len(w.schema) > 0 {
			if ok, why := chat.Qualifies(w.schema); !ok {
				return &chat.ToolRequestError{Kind: chat.KindStrictUnsupported, Reason: fmt.Sprintf("the answer schema cannot be decoded strictly (%s), and WITH strict = \"require\" asks for it", why)}
			}
		}
	}
	return nil
}

// annotateToolCalls stamps each returned call with what the handler knows
// and the backend does not: the binding of the offered tool it names
// (request metadata; whatever the provider returned under that key was
// dropped at the wire), and a problem when its transport shape is wrong.
// Problems, in precedence: truncated (the answer hit the output limit, so
// every call's arguments may be cut off) > not_offered > duplicate_id (the
// second occurrence) > not_json. Arguments are not checked against the
// tool's schema here.
func annotateToolCalls(resp *chat.Response, offered []chat.Tool) {
	byName := make(map[string]chat.Tool, len(offered))
	for _, t := range offered {
		byName[t.Name] = t
	}
	seen := map[string]bool{}
	for i := range resp.ToolCalls {
		c := &resp.ToolCalls[i]
		c.Binding, c.Problem = nil, nil
		t, known := byName[c.Name]
		if known {
			c.Binding = t.Binding
		}
		noJSON := len(strings.TrimSpace(string(c.Arguments))) == 0 || strings.TrimSpace(string(c.Arguments)) == "null"
		switch {
		case resp.Finish == chat.FinishLength:
			c.Problem = &chat.ToolProblem{Code: chat.ProblemTruncated, Message: "the answer hit the output limit, so the arguments may be cut off; re-issue the call"}
		case !known:
			c.Problem = &chat.ToolProblem{Code: chat.ProblemNotOffered, Message: fmt.Sprintf("%q is not a tool this request offered", c.Name)}
		case seen[c.ID]:
			c.Problem = &chat.ToolProblem{Code: chat.ProblemDuplicateID, Message: fmt.Sprintf("id %q repeats an earlier call in this answer", c.ID)}
		case noJSON:
			c.Problem = &chat.ToolProblem{Code: chat.ProblemNotJSON, Message: "the arguments are not a JSON object; raw holds what the model wrote"}
		}
		if noJSON {
			c.Arguments = nil
		}
		seen[c.ID] = true
	}
}

// chatEnvelopeExt is what the envelope carries beyond the shape it had
// before tools: nothing, for a request without tools whose answer made no
// calls (then buildChatEnvelope reproduces the old bytes exactly).
type chatEnvelopeExt struct {
	// into is where the answer lands instead of the top level ("" = top).
	into string
	// tools: the request offered tools or the answer carried calls, so the
	// envelope adds finish, tool_calls, message and response_id.
	tools bool
	// schemaMode, when set, is stamped as `_txc.chat.schema_mode`: how the
	// schema went out (json_schema or json_object). Set only when the
	// request asked for strict decoding, so a plain schema call's envelope
	// is unchanged.
	schemaMode string
}

// buildChatEnvelope assembles the op's output. With a zero ext it is the
// envelope buildChatResponseEnvelope has always produced, key for key:
//
//	{"text", "schema_validated_payload"?, "chat": {"error"}?, "_txc": {"chat": …}}
//
// With ext.tools the answer gains `finish` (when the provider said), the
// annotated `tool_calls`, `message` (the assistant turn, ready to append to
// the next request's messages) and `response_id` (when known). With ext.into
// every answer field lands under that path instead of the top level, while
// `chat.error` and every `_txc.chat.*` stamp stay at the top level: a rule's
// `WHEN @chat.error` works the same either way, and the readers of the
// stamps (modelnote's fallback, the resume projection) find them where they
// always were.
func buildChatEnvelope(resp chat.Response, runErr error, schemaStatus string, validatedPayload json.RawMessage, routing string, ext chatEnvelopeExt) string {
	b := jsonx.NewObject()
	prefix := ""
	if ext.into != "" {
		prefix = ext.into + "."
	}

	if runErr == nil {
		b.Set(prefix+"text", resp.Text)
		if len(validatedPayload) > 0 {
			b.SetRaw(prefix+"schema_validated_payload", string(validatedPayload))
		}
		if ext.tools {
			if resp.Finish != "" {
				b.Set(prefix+"finish", resp.Finish)
			}
			calls := resp.ToolCalls
			if calls == nil {
				calls = []chat.ToolCall{}
			}
			if raw, err := json.Marshal(calls); err == nil {
				b.SetRaw(prefix+"tool_calls", string(raw))
			}
			msg := chat.Message{Role: "assistant", Content: resp.Text, ToolCalls: resp.ToolCalls}
			if raw, err := json.Marshal(msg); err == nil {
				b.SetRaw(prefix+"message", string(raw))
			}
			if resp.ResponseID != "" {
				b.Set(prefix+"response_id", resp.ResponseID)
			}
		}
	} else {
		b.Set("chat.error", chatErrorBody(runErr))
	}

	// Metadata always present, even on error, so the trace + envelope
	// agree.
	b.Set("_txc.chat.provider", resp.Provider)
	b.Set("_txc.chat.model", resp.Model)
	b.Set("_txc.chat.tokens.in", resp.TokensIn)
	b.Set("_txc.chat.tokens.out", resp.TokensOut)
	b.Set("_txc.chat.tokens.cached", resp.TokensCached)
	b.Set("_txc.chat.tokens.reasoning", resp.TokensReasoning)
	b.Set("_txc.chat.latency_ms", resp.LatencyMS)
	b.Set("_txc.chat.retries", resp.Retries)
	if routing != "" {
		b.Set("_txc.chat.routing_decision", routing)
	}
	if schemaStatus != "" {
		b.Set("_txc.chat.schema_validation", schemaStatus)
	}
	if ext.schemaMode != "" {
		b.Set("_txc.chat.schema_mode", ext.schemaMode)
	}
	return b.String()
}

// chatErrorBody is `chat.error`: the code and the message, plus a `detail`
// object for the errors that carry one (the ids of a pending_tool_result).
// Errors without detail keep the two-key shape they always had.
func chatErrorBody(err error) map[string]any {
	body := map[string]any{"message": err.Error()}
	if coded, ok := err.(chat.CodedError); ok {
		body["code"] = coded.Code()
	} else {
		body["code"] = chat.CodeUnknown
	}
	if d, ok := err.(chat.DetailedError); ok {
		if detail := d.ErrorDetail(); detail != nil {
			body["detail"] = detail
		}
	}
	return body
}

// toolCallNames lists the names the calls made, for the model facts.
func toolCallNames(calls []chat.ToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}
