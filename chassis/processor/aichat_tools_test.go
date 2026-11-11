package processor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// --- helpers ---

const searchParams = `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`

// The definition is as txco://caps/list WITH format = "tools" answers it,
// the declaration's `effect` included: that key is the executor's, as the
// binding is, and every test here holds ai://chat to taking a definition
// that carries it (it is read nowhere and sent to no provider).
func toolsMeta(extra string) string {
	return `"tools":[{"name":"web_search","description":"Search the web.","parameters":` + searchParams + `,"binding":{"capability":"web.search"},"effect":"read"}]` + extra
}

func toolUnit(t *testing.T, name string, stub *testBackend) *Unit {
	t.Helper()
	stub.features = chat.ToolFeatures{Tools: true, Parallel: true, Strict: true}
	stub.required = []string{"OPENROUTER_KEY"}
	// The envelope's provider is the response's, as a real backend reports
	// its own name; the stubs report the name they are registered under.
	stub.resp.Provider = name
	withTestBackend(t, name, stub)
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")
	return pu
}

func callsResp(calls ...chat.ToolCall) chat.Response {
	return chat.Response{Provider: "tb", Model: "m", ToolCalls: calls, Finish: chat.FinishToolCalls, TokensIn: 10, TokensOut: 2}
}

// plainBackend has no ToolFeatures method at all: an older backend.
type plainBackend struct{ testBackend }

func (b *plainBackend) ToolFeatures() {} // shadows the embedded method with a different signature

// --- tests ---

func TestExecAIToolsReachBackendAndEnvelope(t *testing.T) {
	stub := &testBackend{resp: callsResp(chat.ToolCall{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"tides"}`)})}
	stub.resp.ResponseID = "chatcmpl-1"
	pu := toolUnit(t, "tb-tools", stub)

	ctx, note := withModelNote(context.Background())
	op := aiChatOp(`{"prompt":"find it","provider":"tb-tools",`+toolsMeta(`,"tool_choice":"auto","parallel":false`)+`}`, `{}`)
	pl, err := pu.ExecAI(ctx, op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if len(stub.sawReq.Tools) != 1 || stub.sawReq.Tools[0].Name != "web_search" || stub.sawReq.ToolChoice != "auto" || stub.sawReq.Parallel == nil || *stub.sawReq.Parallel {
		t.Errorf("request to backend = %+v", stub.sawReq)
	}
	if string(stub.sawReq.Tools[0].Binding) != `{"capability":"web.search"}` {
		t.Errorf("binding not carried on the request: %s", stub.sawReq.Tools[0].Binding)
	}
	raw := pl.Raw
	if gjson.Get(raw, "finish").String() != "tool_calls" || gjson.Get(raw, "text").String() != "" {
		t.Errorf("finish/text = %s / %s", gjson.Get(raw, "finish"), gjson.Get(raw, "text"))
	}
	if gjson.Get(raw, "tool_calls.0.binding.capability").String() != "web.search" || gjson.Get(raw, "tool_calls.0.arguments.q").String() != "tides" {
		t.Errorf("tool_calls = %s", gjson.Get(raw, "tool_calls"))
	}
	if gjson.Get(raw, "tool_calls.0.problem").Exists() {
		t.Errorf("unexpected problem: %s", gjson.Get(raw, "tool_calls.0.problem"))
	}
	if gjson.Get(raw, "message.role").String() != "assistant" || gjson.Get(raw, "message.tool_calls.0.id").String() != "c1" {
		t.Errorf("message = %s", gjson.Get(raw, "message"))
	}
	if gjson.Get(raw, "response_id").String() != "chatcmpl-1" || gjson.Get(raw, "_txc.chat.provider").String() != "tb-tools" {
		t.Errorf("response_id/provider = %s / %s", gjson.Get(raw, "response_id"), gjson.Get(raw, "_txc.chat.provider"))
	}
	// The message can be handed straight back as a transcript turn.
	var msg chat.Message
	if err := json.Unmarshal([]byte(gjson.Get(raw, "message").Raw), &msg); err != nil || len(msg.ToolCalls) != 1 {
		t.Errorf("message does not decode as a chat.Message: %v %+v", err, msg)
	}
	facts := note.facts("ai://chat", pl)
	if facts.ToolsOffered != 1 || len(facts.ToolCalls) != 1 || facts.ToolCalls[0] != "web_search" || facts.Finish != "tool_calls" {
		t.Errorf("facts = %+v", facts)
	}
}

func TestExecAIToolsUnsupportedIsData(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "hi"}, required: []string{"OPENROUTER_KEY"}}
	withTestBackend(t, "tb-notools", stub) // features zero: takes no tools
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-notools",`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatalf("a refusal is data, got Go error %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeToolsUnsupported {
		t.Errorf("code = %q (raw %s)", got, pl.Raw)
	}
	if stub.calls != 0 {
		t.Error("backend was called")
	}
	if stub.sawKey != "" {
		t.Error("a secret was materialized for a refused request")
	}

	// A backend with tools but no parallel control, asked for parallel.
	stub2 := &testBackend{resp: chat.Response{Text: "hi"}, required: []string{"OPENROUTER_KEY"}, features: chat.ToolFeatures{Tools: true}}
	withTestBackend(t, "tb-nopar", stub2)
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-nopar",`+toolsMeta(`,"parallel":false`)+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeToolsUnsupported {
		t.Errorf("parallel on a backend without it: err %v raw %s", err, pl.Raw)
	}
	// strict = require on a backend that cannot.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-nopar",`+toolsMeta(`,"strict":"require"`)+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeStrictUnsupported {
		t.Errorf("strict require on a backend without it: err %v raw %s", err, pl.Raw)
	}
	// strict = prefer is fine there.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-nopar",`+toolsMeta(`,"strict":"prefer"`)+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error").Exists() {
		t.Errorf("strict prefer refused: err %v raw %s", err, pl.Raw)
	}
}

func TestExecAIPlainBackendUnsupported(t *testing.T) {
	stub := &plainBackend{testBackend{resp: chat.Response{Text: "hi"}, required: []string{"OPENROUTER_KEY"}}}
	stub.name = "tb-plain"
	stub.capabilities = []string{"public_execution"}
	chat.Register("tb-plain", func(cfg chat.Config) (chat.Backend, error) { return stub, nil })
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-plain",`+toolsMeta("")+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeToolsUnsupported {
		t.Errorf("err %v raw %s", err, pl.Raw)
	}
	// Without tools it still works as before.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-plain"}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "text").String() != "hi" {
		t.Errorf("plain call: err %v raw %s", err, pl.Raw)
	}
}

func TestExecAIPendingToolResult(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "done"}}
	pu := toolUnit(t, "tb-pending", stub)
	messages := `[{"role":"user","content":"find it"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","name":"web_search","arguments":{"q":"x"}},{"id":"c2","name":"web_search","arguments":{"q":"y"}}]},{"role":"tool","tool_call_id":"c2","content":"second"}]`
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"messages":`+messages+`,"provider":"tb-pending",`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatalf("pending is data, got Go error %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodePendingToolResult {
		t.Errorf("code = %q (raw %s)", got, pl.Raw)
	}
	if ids := gjson.Get(pl.Raw, "chat.error.detail.ids").Array(); len(ids) != 1 || ids[0].String() != "c1" {
		t.Errorf("detail.ids = %s", gjson.Get(pl.Raw, "chat.error.detail.ids"))
	}
	if stub.calls != 0 {
		t.Error("backend was called with an incomplete transcript")
	}
	// Results in any order complete the batch; the backend then sees them.
	messages = `[{"role":"user","content":"find it"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","name":"web_search","arguments":{"q":"x"}},{"id":"c2","name":"web_search","arguments":{"q":"y"}}]},{"role":"tool","tool_call_id":"c2","content":"second"},{"role":"tool","tool_call_id":"c1","content":{"hits":[]}}]`
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"messages":`+messages+`,"provider":"tb-pending",`+toolsMeta("")+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error").Exists() {
		t.Fatalf("complete transcript refused: err %v raw %s", err, pl.Raw)
	}
	if len(stub.sawMessages) != 4 || stub.sawMessages[3].Content != `{"hits":[]}` || stub.sawMessages[3].ToolCallID != "c1" {
		t.Errorf("backend saw %+v", stub.sawMessages)
	}
	if gjson.Get(pl.Raw, "text").String() != "done" || gjson.Get(pl.Raw, "tool_calls").Raw != "[]" {
		t.Errorf("final answer envelope = %s", pl.Raw)
	}
}

func TestExecAIRepairErrorResults(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "done"}}
	pu := toolUnit(t, "tb-repair", stub)
	messages := `[{"role":"user","content":"find it"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","name":"web_search","arguments":{"q":"x"}}]},{"role":"user","content":"never mind"}]`
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"messages":`+messages+`,"provider":"tb-repair","repair":"error_results",`+toolsMeta("")+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error").Exists() {
		t.Fatalf("err %v raw %s", err, pl.Raw)
	}
	if len(stub.sawMessages) != 4 {
		t.Fatalf("backend saw %d messages, want 4 (synthetic result inserted): %+v", len(stub.sawMessages), stub.sawMessages)
	}
	syn := stub.sawMessages[2]
	if syn.Role != "tool" || syn.ToolCallID != "c1" || !syn.IsError || !strings.Contains(syn.Content, "no result") {
		t.Errorf("synthetic result = %+v", syn)
	}
	if stub.sawMessages[3].Role != "user" {
		t.Errorf("order = %+v", stub.sawMessages)
	}
	// A bad repair value is the author's.
	_, err = pu.ExecAI(context.Background(), aiChatOp(`{"messages":`+messages+`,"provider":"tb-repair","repair":"magic"}`, `{}`))
	if err == nil {
		t.Error("bad repair value accepted")
	}
}

func TestExecAIOrphanToolMessageDropsOp(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "done"}}
	pu := toolUnit(t, "tb-orphan", stub)
	for name, messages := range map[string]string{
		"orphan result":         `[{"role":"user","content":"x"},{"role":"tool","tool_call_id":"c9","content":"?"}]`,
		"result answered twice": `[{"role":"assistant","tool_calls":[{"id":"c1","name":"t","arguments":{}}]},{"role":"tool","tool_call_id":"c1","content":"a"},{"role":"tool","tool_call_id":"c1","content":"b"}]`,
		"duplicate id in turn":  `[{"role":"assistant","tool_calls":[{"id":"c1","name":"t","arguments":{}},{"id":"c1","name":"t","arguments":{}}]},{"role":"tool","tool_call_id":"c1","content":"a"}]`,
		"result without id":     `[{"role":"assistant","tool_calls":[{"id":"c1","name":"t","arguments":{}}]},{"role":"tool","content":"a"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"messages":`+messages+`,"provider":"tb-orphan"}`, `{}`))
			if err == nil {
				t.Fatalf("malformed transcript accepted: %s", pl.Raw)
			}
			var te *chat.ToolRequestError
			if !errors.As(err, &te) || te.Kind != chat.KindToolInvalid {
				t.Errorf("err = %v, want tool_invalid", err)
			}
			if gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeToolInvalid {
				t.Errorf("envelope = %s", pl.Raw)
			}
		})
	}
}

func TestCheckTranscript(t *testing.T) {
	call := func(id string) chat.ToolCall {
		return chat.ToolCall{ID: id, Name: "t", Arguments: json.RawMessage(`{}`)}
	}
	result := func(id string) chat.Message { return chat.Message{Role: "tool", ToolCallID: id, Content: "r"} }
	user := chat.Message{Role: "user", Content: "u"}
	cases := []struct {
		name    string
		msgs    []chat.Message
		pending []string
		invalid bool
	}{
		{"no calls", []chat.Message{user, {Role: "assistant", Content: "a"}}, nil, false},
		{"out of order", []chat.Message{user, {Role: "assistant", ToolCalls: []chat.ToolCall{call("a"), call("b"), call("c")}}, result("b"), result("a"), result("c"), {Role: "assistant", Content: "x"}}, nil, false},
		{"two batches", []chat.Message{user, {Role: "assistant", ToolCalls: []chat.ToolCall{call("a")}}, result("a"), {Role: "assistant", ToolCalls: []chat.ToolCall{call("b")}}, result("b")}, nil, false},
		{"outstanding at end", []chat.Message{user, {Role: "assistant", ToolCalls: []chat.ToolCall{call("a"), call("b")}}, result("a")}, []string{"b"}, false},
		{"user before close", []chat.Message{user, {Role: "assistant", ToolCalls: []chat.ToolCall{call("a")}}, user}, []string{"a"}, false},
		{"system before close", []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{call("a")}}, {Role: "system", Content: "s"}}, []string{"a"}, false},
		{"assistant before close", []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{call("a")}}, {Role: "assistant", Content: "x"}}, []string{"a"}, false},
		{"duplicate in turn", []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{call("a"), call("a")}}}, nil, true},
		{"second result", []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{call("a")}}, result("a"), result("a")}, nil, true},
		{"orphan", []chat.Message{user, result("zz")}, nil, true},
		{"call without id", []chat.Message{{Role: "assistant", ToolCalls: []chat.ToolCall{{Name: "t"}}}}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, pending, invalid := checkTranscript(tc.msgs, "")
			if (invalid != nil) != tc.invalid {
				t.Fatalf("invalid = %v", invalid)
			}
			if tc.invalid {
				return
			}
			if (pending != nil) != (tc.pending != nil) {
				t.Fatalf("pending = %v, want %v", pending, tc.pending)
			}
			if pending != nil {
				if strings.Join(pending.IDs, ",") != strings.Join(tc.pending, ",") {
					t.Errorf("pending ids = %v, want %v", pending.IDs, tc.pending)
				}
				if pending.Code() != chat.CodePendingToolResult {
					t.Errorf("code = %s", pending.Code())
				}
				// With repair the same transcript is completed, in call order,
				// where the results should have been.
				repaired, p2, i2 := checkTranscript(tc.msgs, chatRepairErrorResults)
				if p2 != nil || i2 != nil {
					t.Fatalf("repair: %v %v", p2, i2)
				}
				if len(repaired) != len(tc.msgs)+len(tc.pending) {
					t.Errorf("repaired has %d messages, want %d", len(repaired), len(tc.msgs)+len(tc.pending))
				}
				return
			}
			if len(out) != len(tc.msgs) {
				t.Errorf("out = %d messages, want %d", len(out), len(tc.msgs))
			}
		})
	}
}

func TestExecAIBindingFromRequestNotProvider(t *testing.T) {
	// The backend (standing in for a provider) returns bindings of its own
	// and a call to a tool that was never offered.
	stub := &testBackend{resp: callsResp(
		chat.ToolCall{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"x"}`), Binding: json.RawMessage(`{"capability":"mail.send"}`)},
		chat.ToolCall{ID: "c2", Name: "mail_send", Arguments: json.RawMessage(`{"to":"x"}`), Binding: json.RawMessage(`{"capability":"mail.send"}`)},
	)}
	pu := toolUnit(t, "tb-bind", stub)
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-bind",`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(pl.Raw, "tool_calls.0.binding.capability").String(); got != "web.search" {
		t.Errorf("binding = %q, want the request's web.search", got)
	}
	if gjson.Get(pl.Raw, "tool_calls.1.binding").Exists() {
		t.Errorf("a not_offered call carries a binding: %s", gjson.Get(pl.Raw, "tool_calls.1"))
	}
	if gjson.Get(pl.Raw, "tool_calls.1.problem.code").String() != chat.ProblemNotOffered {
		t.Errorf("problem = %s", gjson.Get(pl.Raw, "tool_calls.1.problem"))
	}
}

func TestExecAIToolProblems(t *testing.T) {
	cases := []struct {
		name  string
		resp  chat.Response
		want  []string // problem codes per call, "" for none
		nulls []bool   // arguments null?
	}{
		{"truncated wins", chat.Response{Finish: chat.FinishLength, ToolCalls: []chat.ToolCall{
			{ID: "c1", Name: "web_search", Raw: `{"q": "cu`},
			{ID: "c2", Name: "nope", Arguments: json.RawMessage(`{}`)},
		}}, []string{chat.ProblemTruncated, chat.ProblemTruncated}, []bool{true, false}},
		{"not offered then duplicate then not json", chat.Response{Finish: chat.FinishToolCalls, ToolCalls: []chat.ToolCall{
			{ID: "c1", Name: "nope", Arguments: json.RawMessage(`{}`)},
			{ID: "c2", Name: "web_search", Arguments: json.RawMessage(`{"q":"a"}`)},
			{ID: "c2", Name: "web_search", Arguments: json.RawMessage(`{"q":"b"}`)},
			{ID: "c3", Name: "web_search", Raw: `[1,2]`},
			{ID: "c4", Name: "web_search", Arguments: json.RawMessage(`null`)},
		}}, []string{chat.ProblemNotOffered, "", chat.ProblemDuplicateID, chat.ProblemNotJSON, chat.ProblemNotJSON}, []bool{false, false, false, true, true}},
		{"finish stop with calls still counts", chat.Response{Finish: chat.FinishStop, ToolCalls: []chat.ToolCall{{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"a"}`)}}}, []string{""}, []bool{false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.resp.Provider, tc.resp.Model = "tb", "m"
			stub := &testBackend{resp: tc.resp}
			pu := toolUnit(t, "tb-prob-"+strings.ReplaceAll(tc.name, " ", "-"), stub)
			pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-prob-`+strings.ReplaceAll(tc.name, " ", "-")+`",`+toolsMeta("")+`}`, `{}`))
			if err != nil {
				t.Fatal(err)
			}
			calls := gjson.Get(pl.Raw, "tool_calls").Array()
			if len(calls) != len(tc.want) {
				t.Fatalf("calls = %d, want %d: %s", len(calls), len(tc.want), pl.Raw)
			}
			for i, w := range tc.want {
				got := calls[i].Get("problem.code").String()
				if got != w {
					t.Errorf("call %d problem = %q, want %q", i, got, w)
				}
				if isNull := calls[i].Get("arguments").Type == gjson.Null; isNull != tc.nulls[i] {
					t.Errorf("call %d arguments null = %v, want %v (%s)", i, isNull, tc.nulls[i], calls[i].Raw)
				}
				if !calls[i].Get("arguments").Exists() {
					t.Errorf("call %d: arguments omitted; it must be present (null or an object)", i)
				}
			}
			if gjson.Get(pl.Raw, "chat.error").Exists() {
				t.Errorf("a problem is not a chat error: %s", pl.Raw)
			}
		})
	}
}

func TestExecAIToolsSkipSchemaValidationOnCalls(t *testing.T) {
	stub := &testBackend{resp: callsResp(chat.ToolCall{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"x"}`)})}
	pu := toolUnit(t, "tb-schema-calls", stub)
	schema := `"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-schema-calls",`+schema+`,`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(pl.Raw, "chat.error").Exists() || gjson.Get(pl.Raw, "_txc.chat.schema_validation").Exists() {
		t.Errorf("schema applied to a tool-calling answer: %s", pl.Raw)
	}
	// A final text answer is validated as before.
	stub.resp = chat.Response{Provider: "tb", Model: "m", Text: `{"answer":"a"}`, Finish: chat.FinishStop}
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-schema-calls",`+schema+`,`+toolsMeta("")+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "_txc.chat.schema_validation").String() != "ok" || gjson.Get(pl.Raw, "schema_validated_payload.answer").String() != "a" {
		t.Errorf("final answer: err %v raw %s", err, pl.Raw)
	}
}

func TestExecAIIntoPlacesAnswerKeepsTxcTopLevel(t *testing.T) {
	stub := &testBackend{resp: callsResp(chat.ToolCall{ID: "c1", Name: "web_search", Arguments: json.RawMessage(`{"q":"x"}`)})}
	pu := toolUnit(t, "tb-into", stub)
	ctx, note := withModelNote(context.Background())
	pl, err := pu.ExecAI(ctx, aiChatOp(`{"prompt":"x","provider":"tb-into","into":"_pi.turn",`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := pl.Raw
	if gjson.Get(raw, "_pi.turn.tool_calls.0.id").String() != "c1" || gjson.Get(raw, "_pi.turn.finish").String() != "tool_calls" || !gjson.Get(raw, "_pi.turn.text").Exists() {
		t.Errorf("answer not under into: %s", raw)
	}
	for _, k := range []string{"text", "tool_calls", "finish", "message"} {
		if gjson.Get(raw, k).Exists() {
			t.Errorf("%s also at the top level", k)
		}
	}
	if gjson.Get(raw, "_txc.chat.provider").String() != "tb-into" || gjson.Get(raw, "_pi.turn._txc").Exists() {
		t.Errorf("_txc.chat.* moved: %s", raw)
	}
	if f := note.facts("ai://chat", pl); f.Provider != "tb-into" {
		t.Errorf("facts provider = %q", f.Provider)
	}
	// A provider error with into: chat.error top-level, nothing under into.
	stub.err = &chat.ProviderHTTPError{StatusCode: 503, Body: "down"}
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-into","into":"_pi.turn",`+toolsMeta("")+`}`, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeProviderHTTP || gjson.Get(pl.Raw, "_pi").Exists() {
		t.Errorf("error with into: %s", pl.Raw)
	}
	stub.err = nil
	// into without tools places text under it and nothing else new.
	stub.resp = chat.Response{Provider: "tb", Model: "m", Text: "hi"}
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-into","into":"_ans"}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "_ans.text").String() != "hi" || gjson.Get(pl.Raw, "text").Exists() || gjson.Get(pl.Raw, "_ans.tool_calls").Exists() {
		t.Errorf("into without tools: err %v raw %s", err, pl.Raw)
	}
}

func TestExecAIIntoRefusesTxcAndChat(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "hi"}}
	pu := toolUnit(t, "tb-into-bad", stub)
	for _, into := range []string{"_txc.x", "@tenant", "chat", "chat.answer", "_txc_op_debug"} {
		_, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-into-bad","into":"`+into+`"}`, `{}`))
		if err == nil {
			t.Errorf("into %q accepted", into)
		}
	}
	if stub.calls != 0 {
		t.Error("backend called for a refused into")
	}
}

func TestExecAIPromptFormWithTools(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Provider: "tb", Model: "m", Text: "no tool needed", Finish: chat.FinishStop}}
	pu := toolUnit(t, "tb-prompt-tools", stub)
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"system":"be brief","prompt":"hello {{@name}}","provider":"tb-prompt-tools",`+toolsMeta("")+`}`, `{"_txc":{"name":"ann"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.sawMessages) != 2 || stub.sawMessages[1].Content != "hello ann" || len(stub.sawReq.Tools) != 1 {
		t.Errorf("backend saw %+v tools %d", stub.sawMessages, len(stub.sawReq.Tools))
	}
	// Tools were offered, so the envelope says how it ended and that no call was made.
	if gjson.Get(pl.Raw, "text").String() != "no tool needed" || gjson.Get(pl.Raw, "finish").String() != "stop" || gjson.Get(pl.Raw, "tool_calls").Raw != "[]" {
		t.Errorf("envelope = %s", pl.Raw)
	}
}

func TestExecAIToolInvalidDropsOp(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "hi"}}
	pu := toolUnit(t, "tb-invalid", stub)
	cases := map[string]string{
		"a name instead of a definition": `"tools":["web.search"]`,
		"tools not an array":             `"tools":{"name":"x"}`,
		"dotted name":                    `"tools":[{"name":"web.search","parameters":{"type":"object"}}]`,
		"no parameters":                  `"tools":[{"name":"web_search"}]`,
		"choice names unoffered tool":    toolsMeta(`,"tool_choice":"mail_send"`),
		"choice without tools":           `"tool_choice":"auto"`,
		"parallel without tools":         `"parallel":true`,
		"parallel not a bool":            toolsMeta(`,"parallel":"yes"`),
		"bad strict":                     `"strict":"always"`,
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-invalid",`+meta+`}`, `{}`))
			if err == nil {
				t.Fatalf("accepted: %s", pl.Raw)
			}
			if code := gjson.Get(pl.Raw, "chat.error.code").String(); !strings.HasPrefix(code, "txco_chat_") {
				t.Errorf("envelope code = %q", code)
			}
		})
	}
	if stub.calls != 0 {
		t.Error("backend called for an invalid request")
	}
}

func TestExecAINoToolsEnvelopeUnchanged(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Provider: "tb-frozen", Model: "m", Text: "hello back", TokensIn: 10, TokensOut: 5, LatencyMS: 42}}
	pu := toolUnit(t, "tb-frozen", stub)
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"hi","provider":"tb-frozen"}`, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	want := buildChatResponseEnvelopeFrozen(stub.resp, nil, "", nil, "provider-override")
	if pl.Raw != want {
		t.Errorf("no-tools envelope changed\n got: %s\nwant: %s", pl.Raw, want)
	}
	// Including when the backend reports a finish it was not asked about tools for.
	stub.resp.Finish = chat.FinishStop
	pl, _ = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"hi","provider":"tb-frozen"}`, `{}`))
	if pl.Raw != want {
		t.Errorf("finish leaked into a no-tools envelope: %s", pl.Raw)
	}
	// And a debug stamp on a plain call carries no tool fields.
	pl, _ = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"hi","provider":"tb-frozen","debug":true}`, `{}`))
	if gjson.Get(pl.Raw, opDebugField+".tools_sent").Exists() {
		t.Error("tools_sent stamped on a plain call")
	}
}

func TestExecAIStrictRequireNeedsQualifyingSchemas(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: "hi", Finish: chat.FinishStop}}
	pu := toolUnit(t, "tb-strict", stub)
	loose := `"tools":[{"name":"loose","parameters":{"type":"object","properties":{"anything":{}}}}]`
	// require + a tool that cannot be decoded strictly: refused as data.
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-strict","strict":"require",`+loose+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeStrictUnsupported {
		t.Errorf("require with a loose tool: err %v raw %s", err, pl.Raw)
	}
	if stub.calls != 0 {
		t.Error("backend called")
	}
	// prefer with the same tool goes through.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x","provider":"tb-strict","strict":"prefer",`+loose+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error").Exists() || stub.sawReq.Strict != chat.StrictPrefer {
		t.Errorf("prefer: err %v raw %s strict %q", err, pl.Raw, stub.sawReq.Strict)
	}
	// require with a qualifying tool and schema goes through.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-strict","strict":"require",`+toolsMeta("")+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error").Exists() {
		t.Errorf("require with a typed tool: err %v raw %s", err, pl.Raw)
	}
	// require + an answer schema that cannot be strict: refused.
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-strict","strict":"require","schema":{"type":"object","properties":{"a":{}}}}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "chat.error.code").String() != chat.CodeStrictUnsupported {
		t.Errorf("require with a loose schema: err %v raw %s", err, pl.Raw)
	}
}

func TestExecAISchemaModeStampOnlyWithStrict(t *testing.T) {
	stub := &testBackend{resp: chat.Response{Text: `{"answer":"a"}`, Finish: chat.FinishStop, SchemaMode: "json_schema"}}
	pu := toolUnit(t, "tb-mode", stub)
	schema := `"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`
	// Without strict the stamp is absent even though the backend reported a mode:
	// a plain schema call's envelope is unchanged.
	pl, err := pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-mode",`+schema+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "_txc.chat.schema_mode").Exists() {
		t.Errorf("plain schema call: err %v raw %s", err, pl.Raw)
	}
	want := buildChatResponseEnvelopeFrozen(stub.resp, nil, "ok", json.RawMessage(`{"answer":"a"}`), "provider-override")
	if pl.Raw != want {
		t.Errorf("plain schema envelope changed\n got: %s\nwant: %s", pl.Raw, want)
	}
	pl, err = pu.ExecAI(context.Background(), aiChatOp(`{"prompt":"x json","provider":"tb-mode","strict":"prefer",`+schema+`}`, `{}`))
	if err != nil || gjson.Get(pl.Raw, "_txc.chat.schema_mode").String() != "json_schema" || gjson.Get(pl.Raw, "_txc.chat.schema_validation").String() != "ok" {
		t.Errorf("strict schema call: err %v raw %s", err, pl.Raw)
	}
}

var _ = secrets.SecretBag{}
