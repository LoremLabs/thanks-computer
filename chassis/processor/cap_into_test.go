package processor

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// `WITH cap_into` (cap.go capPlaceOf): the facts about one call (name,
// status, ms, rid, error) land at a path of the op's own instead of the
// shared `cap`, so calls made side by side in one scope can each be read.

// Up to the parent: an answer and a refusal both write their facts at the
// named path, and nothing at `cap`.
func TestExecCapIntoNamesWhereTheFactsLand(t *testing.T) {
	p, srv := newParent(t)
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL, OpPayloadMax: 1 << 20}}
	ctx := WithRunGrant(context.Background(), "rg1.tok")

	payload, err := pu.ExecCap(ctx, capOp("card.note", `{"into":"_r1","cap_into":"_m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(payload.Raw, "_r1.seq").Int() != 7 {
		t.Errorf("the output is not at into: %s", payload.Raw)
	}
	m := gjson.Get(payload.Raw, "_m1")
	if m.Get("name").String() != "card.note" || m.Get("status").Int() != 200 || !m.Get("ms").Exists() ||
		m.Get("rid").String() != "rid-parent-1" || m.Get("error").Exists() {
		t.Errorf("the facts of an answered call: %s", m.Raw)
	}
	if gjson.Get(payload.Raw, "cap").Exists() {
		t.Errorf("a call with its own cap_into wrote at the shared path: %s", payload.Raw)
	}

	p.mu.Lock()
	p.status, p.answer = http.StatusUnprocessableEntity, `{"ok":false,"error":{"code":"no_such_card","message":"c9"}}`
	p.mu.Unlock()
	// `.`-led, as a rule writes a path; and nested.
	payload, err = pu.ExecCap(ctx, capOp("card.note", `{"into":"_r1","cap_into":"._calls.one"}`))
	if err != nil {
		t.Fatal(err)
	}
	m = gjson.Get(payload.Raw, "_calls.one")
	if m.Get("error.code").String() != "no_such_card" || m.Get("error.message").String() != "c9" ||
		m.Get("error.status").Int() != 422 || m.Get("name").String() != "card.note" || m.Get("rid").String() != "rid-parent-1" {
		t.Errorf("the facts of a refused call: %s", payload.Raw)
	}
	if gjson.Get(payload.Raw, "_r1").Raw != "{}" || gjson.Get(payload.Raw, "cap").Exists() {
		t.Errorf("a refused call leaves an empty output and nothing at cap: %s", payload.Raw)
	}

	// Without it, as before: the shared path.
	payload, _ = pu.ExecCap(ctx, capOp("card.note", `{"into":"_r1"}`))
	if gjson.Get(payload.Raw, "cap.error.code").String() != "no_such_card" {
		t.Errorf("the default place moved: %s", payload.Raw)
	}
}

// A cap_into that cannot be used drops the op as an authoring error: a quiet
// fall back to `cap` would put the facts where a neighbour's are.
func TestExecCapIntoRefusesWhatItCannotPlace(t *testing.T) {
	p, srv := newParent(t)
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL, OpPayloadMax: 1 << 20}}
	ctx := WithRunGrant(context.Background(), "rg1.tok")
	for _, tc := range []struct{ what, meta, says string }{
		{"empty", `{"cap_into":""}`, "non-empty"},
		{"null (an absent path in the WITH value)", `{"cap_into":null}`, "non-empty"},
		{"not a string", `{"cap_into":7}`, "non-empty"},
		{"the reserved tree", `{"cap_into":"_txc.x"}`, "_txc"},
		{"the reserved tree, as @", `{"cap_into":"@x"}`, "_txc"},
		{"an index that pads an array past the limit", `{"cap_into":"_m.99999999"}`, "usable"},
		{"into itself", `{"into":"_r","cap_into":"_r"}`, "overlap"},
		{"inside into", `{"into":"_r","cap_into":"_r.about"}`, "overlap"},
		{"around into", `{"into":"_r.output","cap_into":"_r"}`, "overlap"},
		{"inside the default into", `{"cap_into":"_cap.about"}`, "overlap"},
	} {
		payload, err := pu.ExecCap(ctx, capOp("card.note", tc.meta))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: err = %v, want one that says %q (%s)", tc.what, err, tc.says, payload.Raw)
		}
		if payload.Raw != `{}` {
			t.Errorf("%s: a dropped op wrote %s", tc.what, payload.Raw)
		}
	}
	if p.path != "" {
		t.Errorf("an op that was dropped called the parent")
	}
	// A sibling path that only starts with the same letters is not inside.
	if _, err := pu.ExecCap(ctx, capOp("card.note", `{"into":"_r","cap_into":"_r_about"}`)); err != nil {
		t.Errorf("_r_about is beside _r, not inside it: %v", err)
	}
}

// What the option is for: four calls in ONE scope, answered here. Two are
// answered, one is refused by its stack, one gets no answer. Each is read at
// its own paths; with the shared `cap` only the last merged could be.
func TestExecCapIntoKeepsCallsOfOneScopeApart(t *testing.T) {
	pu, ctx := capUnit(t)
	pu.Dbc.Db.SetMaxOpenConns(1) // concurrent lookups on a :memory: handle
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" && @cap.input.do == "send" EMIT ._cap.output = &object("to", @cap.input.to)`)
	seedAcmeOp(t, pu, "svc", 7000, "refuse", `WHEN @src == "cap" && @cap.input.do == "refuse" EMIT ._cap.error = &object("code", "no_such_card", "message", @cap.input.to)`)
	seedAcmeOp(t, pu, "quiet", 100, "shrug", `EMIT .nothing = true`)
	seedAcmeOp(t, pu, "c", 100, "a", `WITH input = &object("do", "send", "to", "a"), into = "_r.a", cap_into = "_m.a" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 100, "b", `WITH input = &object("do", "refuse", "to", "b"), into = "_r.b", cap_into = "_m.b" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 100, "c", `WITH into = "_r.c", cap_into = "_m.c" EXEC "cap://quiet.thing"`)
	seedAcmeOp(t, pu, "c", 100, "d", `WITH input = &object("do", "send", "to", "d"), into = "_r.d", cap_into = "_m.d" EXEC "cap://mail.send"`)
	// The next scope gates on each call by its own facts.
	seedAcmeOp(t, pu, "c", 200, "failed-b", `WHEN exists ._m.b.error EMIT .said.b = ._m.b.error.code`)
	seedAcmeOp(t, pu, "c", 200, "failed-c", `WHEN exists ._m.c.error EMIT .said.c = ._m.c.error.code`)
	seedAcmeOp(t, pu, "c", 200, "failed-a", `WHEN exists ._m.a.error EMIT .said.a = "failed"`)
	seedAcmeOp(t, pu, "c", 200, "failed-d", `WHEN exists ._m.d.error EMIT .said.d = "failed"`)

	final := runCap(t, pu, ctx, "c/0")
	if gjson.Get(final, "_r.a.to").String() != "a" || gjson.Get(final, "_r.d.to").String() != "d" ||
		gjson.Get(final, "_r.b").Raw != "{}" || gjson.Get(final, "_r.c").Raw != "{}" {
		t.Errorf("the outputs: %s", gjson.Get(final, "_r").Raw)
	}
	for _, k := range []string{"a", "d"} {
		m := gjson.Get(final, "_m."+k)
		if m.Get("name").String() != "mail.send" || m.Get("status").Int() != 200 || !m.Get("ms").Exists() || m.Get("error").Exists() {
			t.Errorf("answered call %s: %s", k, m.Raw)
		}
	}
	if m := gjson.Get(final, "_m.b"); m.Get("error.code").String() != "no_such_card" || m.Get("error.message").String() != "b" ||
		m.Get("error.status").Int() != 422 || m.Get("name").String() != "mail.send" || !m.Get("ms").Exists() {
		t.Errorf("the refused call: %s", m.Raw)
	}
	if m := gjson.Get(final, "_m.c"); m.Get("error.code").String() != "txco_cap_unknown" || m.Get("error.status").Int() != 404 || m.Get("name").String() != "quiet.thing" {
		t.Errorf("the unanswered call: %s", m.Raw)
	}
	if gjson.Get(final, "cap").Exists() {
		t.Errorf("something was written at the shared path: %s", gjson.Get(final, "cap").Raw)
	}
	if gjson.Get(final, "said").Raw != `{"b":"no_such_card","c":"txco_cap_unknown"}` && gjson.Get(final, "said").Raw != `{"c":"txco_cap_unknown","b":"no_such_card"}` {
		t.Errorf("the next scope read each call's own error: said = %s", gjson.Get(final, "said").Raw)
	}
}

// Under a run grant: a call the gateway refuses says so at its own path, and
// a neighbour the gateway allows is not touched by it.
func TestExecCapIntoUnderAGrant(t *testing.T) {
	pu, ctx := capUnit(t)
	pu.Dbc.Db.SetMaxOpenConns(1) // concurrent lookups on a :memory: handle
	pu.Grants = &stubCapGrants{decided: map[string]CapDecision{
		"mail.send":   allowedFor("run-1"),
		"quiet.thing": {Identified: true, Held: true, Tenant: "acme"},
	}}
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" EMIT ._cap.output.ran = true`)
	seedAcmeOp(t, pu, "quiet", 100, "never", `EMIT ._cap.output.ran = true`)
	seedAcmeOp(t, pu, "c", 100, "send", `WITH into = "_sent", cap_into = "_sent_cap" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 100, "held", `WITH into = "_held", cap_into = "_held_cap" EXEC "cap://quiet.thing"`)
	final := runCap(t, pu, ctx, "c/0")
	if !gjson.Get(final, "_sent.ran").Bool() || gjson.Get(final, "_sent_cap.error").Exists() || gjson.Get(final, "_sent_cap.status").Int() != 200 {
		t.Errorf("the allowed call: %s", final)
	}
	if gjson.Get(final, "_held.ran").Exists() || gjson.Get(final, "_held_cap.error.code").String() != "txco_cap_held" ||
		gjson.Get(final, "_held_cap.error.status").Int() != 409 || !gjson.Get(final, "_held_cap.ms").Exists() {
		t.Errorf("the held call: %s", final)
	}
	if gjson.Get(final, "cap").Exists() {
		t.Errorf("something was written at the shared path: %s", final)
	}
}
