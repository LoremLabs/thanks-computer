package processor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// A run UNDER a run grant — a token in its context — calling a name
// declared here is decided at the grant gateway first, as a node's call is
// at the inlet, and the declaring stack is stamped with the run that asked
// (cap.go execCapDecided). A callee under `call://… WITH grant` is such a
// run (call.go): the gateway signs it a token of its own, which its cap://
// calls present.

// stubCapGrants is a gateway that signs what it is told and decides by
// name, recording every token and name it was handed.
type stubCapGrants struct {
	mu      sync.Mutex
	token   string // what ForCall signs
	callErr error  // ForCall's refusal
	decided map[string]CapDecision
	asked   []string // "token name" per Decide
	forCall []string // "tenant stack grant" per ForCall
}

func (s *stubCapGrants) ForExec(context.Context, string, string, string, string, []string) (map[string]string, [][]byte, error) {
	return nil, nil, errors.New("not in these tests")
}

func (s *stubCapGrants) ForCall(_ context.Context, tenant, stack, grantID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forCall = append(s.forCall, tenant+" "+stack+" "+grantID)
	if s.callErr != nil {
		return "", s.callErr
	}
	return s.token, nil
}

func (s *stubCapGrants) Decide(_ context.Context, token, name string, _ json.RawMessage) CapDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, token+" "+name)
	if d, ok := s.decided[name]; ok {
		return d
	}
	return CapDecision{Identified: true, Allowed: false, Reason: "not listed", Tenant: "acme"}
}

func allowedFor(run string) CapDecision {
	d := CapDecision{Allowed: true, Identified: true, Tenant: "acme", Run: run, Grant: "rgr_9", Generation: 2, Stack: "loop", Workspace: "ws_pony", Trace: "tr_1"}
	d.Principal.ID, d.Principal.Kind = "service:pony-x", "service"
	return d
}

// A name declared here, under a grant: allowed runs the declaring stack
// with the grant's facts; held, refused, unidentified and unavailable are
// the inlet's answers, as data, and the stack never runs.
func TestExecCapUnderAGrantIsDecidedFirst(t *testing.T) {
	pu, ctx := capUnit(t) // ctx carries the token "tok-from-a-node"
	stub := &stubCapGrants{decided: map[string]CapDecision{
		"mail.send":   allowedFor("run-9"),
		"slow.thing":  {Identified: true, Held: true, Tenant: "acme", Run: "run-9"},
		"quiet.thing": {Identified: false, Reason: "token"},
		"outer.thing": {Unavailable: true, Reason: "identity store"},
	}}
	pu.Grants = stub
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" AND @cap.name == "mail.send" `+
		`EMIT ._cap.output = &object("run", @cap.run, "grant", @cap.grant, "gen", @cap.generation, "who", @cap.principal.id, `+
		`"kind", @cap.principal.kind, "minted", @cap.stack, "ws", @cap.workspace, "trace", @cap.trace, "to", @cap.input.to)`)
	seedAcmeOp(t, pu, "slow", 0, "never", `EMIT ._cap.output.ran = true`)
	seedAcmeOp(t, pu, "quiet", 0, "never", `EMIT ._cap.output.ran = true`)
	seedAcmeOp(t, pu, "outer", 0, "never", `EMIT ._cap.output.ran = true`)
	seedAcmeOp(t, pu, "c", 100, "send", `WITH input = &object("to", "a@b.example"), into = "_sent" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 200, "held", `WITH into = "_held" EXEC "cap://slow.thing"`)
	seedAcmeOp(t, pu, "c", 300, "bad", `WITH into = "_bad" EXEC "cap://quiet.thing"`)
	seedAcmeOp(t, pu, "c", 400, "down", `WITH into = "_down" EXEC "cap://outer.thing"`)

	final := runCap(t, pu, ctx, "c/0")
	sent := gjson.Get(final, "_sent")
	if sent.Get("run").String() != "run-9" || sent.Get("grant").String() != "rgr_9" || sent.Get("gen").Int() != 2 ||
		sent.Get("who").String() != "service:pony-x" || sent.Get("kind").String() != "service" || sent.Get("minted").String() != "loop" ||
		sent.Get("ws").String() != "ws_pony" || sent.Get("trace").String() != "tr_1" || sent.Get("to").String() != "a@b.example" {
		t.Errorf("the allowed call ran without the grant's facts: %s", sent.Raw)
	}
	// Each refusal is the inlet's: the code, the status; no output, no run.
	for _, c := range []struct {
		key, code string
		status    int64
	}{
		{"_held", "txco_cap_held", 409}, {"_bad", "txco_cap_denied", 401}, {"_down", "txco_cap_unavailable", 503},
	} {
		if gjson.Get(final, c.key+".ran").Exists() {
			t.Errorf("%s: the declaring stack ran: %s", c.key, final)
		}
	}
	// The failures land at cap.error, in order; the last one is outer.thing.
	if got := gjson.Get(final, "cap.error.code").String(); got != "txco_cap_unavailable" {
		t.Errorf("cap.error.code = %q, want txco_cap_unavailable (the last call): %s", got, gjson.Get(final, "cap").Raw)
	}
	if got := gjson.Get(final, "cap.error.status").Int(); got != 503 {
		t.Errorf("cap.error.status = %d, want 503", got)
	}
	// Every call presented the run's token to the gateway, by name.
	want := []string{"tok-from-a-node mail.send", "tok-from-a-node slow.thing", "tok-from-a-node quiet.thing", "tok-from-a-node outer.thing"}
	if len(stub.asked) != len(want) {
		t.Fatalf("decided %v, want %v", stub.asked, want)
	}
	for i := range want {
		if stub.asked[i] != want[i] {
			t.Errorf("decided[%d] = %q, want %q", i, stub.asked[i], want[i])
		}
	}
}

// Held and refused, one at a time, so each answer's code and status is
// checked where it lands.
func TestExecCapUnderAGrantRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		dec    CapDecision
		code   string
		status int64
	}{
		{"held", CapDecision{Identified: true, Held: true, Tenant: "acme"}, "txco_cap_held", 409},
		{"refused", CapDecision{Identified: true, Allowed: false, Tenant: "acme", Reason: "no standing grant"}, "txco_cap_denied", 403},
		{"unidentified", CapDecision{Identified: false}, "txco_cap_denied", 401},
		{"another tenant", func() CapDecision { d := allowedFor("run-1"); d.Tenant = "other"; return d }(), "txco_cap_denied", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			pu, ctx := capUnit(t)
			pu.Grants = &stubCapGrants{decided: map[string]CapDecision{"mail.send": c.dec}}
			seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" EMIT ._cap.output.ran = true`)
			seedAcmeOp(t, pu, "c", 100, "send", `WITH into = "_sent" EXEC "cap://mail.send"`)
			final := runCap(t, pu, ctx, "c/0")
			if gjson.Get(final, "_sent.ran").Exists() {
				t.Errorf("the declaring stack ran: %s", final)
			}
			if got := gjson.Get(final, "cap.error.code").String(); got != c.code {
				t.Errorf("cap.error.code = %q, want %q: %s", got, c.code, final)
			}
			if got := gjson.Get(final, "cap.error.status").Int(); got != c.status {
				t.Errorf("cap.error.status = %d, want %d", got, c.status)
			}
		})
	}
}

// No gateway on this chassis (a node): a name declared here is still
// answered here with the request's authority, token or not — only the
// parent can check that token.
func TestExecCapUnderAGrantWithoutAGatewayRunsHere(t *testing.T) {
	pu, ctx := capUnit(t)
	pu.Grants = nil
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" EMIT ._cap.output = &object("ran", true, "run", @cap.run)`)
	seedAcmeOp(t, pu, "c", 100, "send", `WITH into = "_sent" EXEC "cap://mail.send"`)
	final := runCap(t, pu, ctx, "c/0")
	if !gjson.Get(final, "_sent.ran").Bool() || gjson.Get(final, "_sent.run").String() != "" {
		t.Errorf("_sent = %s", gjson.Get(final, "_sent").Raw)
	}
}

// `call://<stack> WITH grant`: the callee runs under a token the gateway
// signs for that grant — not the caller's — and its cap:// calls present
// it. A grant the gateway refuses, or no gateway, fails the call as data,
// before the callee runs.
func TestCallWithGrantRunsTheCalleeUnderIt(t *testing.T) {
	pu, ctx := capUnit(t)
	stub := &stubCapGrants{token: "tok-for-the-callee", decided: map[string]CapDecision{"mail.send": allowedFor("run-7")}}
	pu.Grants = stub
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" AND @cap.name == "mail.send" `+
		`EMIT ._cap.output = &object("run", @cap.run, "to", @cap.input.to)`)
	seedAcmeOp(t, pu, "agent", 100, "ask", `WHEN @src == "call" WITH input = &object("to", @call.input.to), into = "_sent" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "agent", 200, "answer", `EMIT ._call.output = &object("sent", ._sent, "code", .cap.error.code)`)
	seedAcmeOp(t, pu, "c", 100, "call", `WITH grant = "rgr_7", input = &object("to", "p@q.example"), into = "_run" EXEC "call://agent"`)

	final := runCap(t, pu, ctx, "c/0")
	res := gjson.Get(final, "_run")
	if !res.Get("ok").Bool() || res.Get("output.sent.run").String() != "run-7" || res.Get("output.sent.to").String() != "p@q.example" {
		t.Errorf("_run = %s", res.Raw)
	}
	if len(stub.forCall) != 1 || stub.forCall[0] != "acme c rgr_7" {
		t.Errorf("ForCall was asked %v, want [acme c rgr_7]", stub.forCall)
	}
	if len(stub.asked) != 1 || stub.asked[0] != "tok-for-the-callee mail.send" {
		t.Errorf("the callee presented %v, want its own token", stub.asked)
	}

	// The gateway refuses the grant: the callee never runs.
	stub2 := &stubCapGrants{callErr: errors.New("run grant rgr_7 was minted by another stack")}
	pu.Grants = stub2
	final = runCap(t, pu, ctx, "c/0")
	res = gjson.Get(final, "_run")
	if res.Get("ok").Bool() || res.Get("error.code").String() != "txco_call_grant" || res.Get("output.sent").Exists() {
		t.Errorf("refused grant: _run = %s", res.Raw)
	}
	if len(stub2.asked) != 0 {
		t.Errorf("the callee ran: %v", stub2.asked)
	}

	// No gateway: the same, saying so.
	pu.Grants = nil
	final = runCap(t, pu, ctx, "c/0")
	if got := gjson.Get(final, "_run.error.code").String(); got != "txco_call_grant" {
		t.Errorf("no gateway: error.code = %q: %s", got, gjson.Get(final, "_run").Raw)
	}
}
