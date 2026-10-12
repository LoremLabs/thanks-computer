package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// A `cap://` whose name a stack of the request's tenant declares runs here,
// in process, on the call engine (cap.go execCapLocal): the second way into
// the engine (docs/todo-stack-call.md §5 in the service repo).

// capLookupFunc adapts a function to CapLookup.
type capLookupFunc func(ctx context.Context, tenant, name string) (string, *capdecl.Decl, error)

func (f capLookupFunc) Lookup(ctx context.Context, tenant, name string) (string, *capdecl.Decl, error) {
	return f(ctx, tenant, name)
}

// declares makes a lookup that knows the given names, each answered by its
// stack at its entry, and answers ErrNotDeclared for any other.
func declares(decls map[string]struct {
	stack string
	decl  *capdecl.Decl
}) CapLookup {
	return capLookupFunc(func(_ context.Context, tenant, name string) (string, *capdecl.Decl, error) {
		if tenant == "" {
			return "", nil, capdecl.ErrNotDeclared
		}
		if d, ok := decls[name]; ok {
			return d.stack, d.decl, nil
		}
		return "", nil, capdecl.ErrNotDeclared
	})
}

// capUnit is a tenant-pinned unit whose `svc` stack declares mail.send at
// scope 7000, with a parent that must never be contacted.
func capUnit(t *testing.T) (*Unit, context.Context) {
	t.Helper()
	pu, _ := newTestUnit(t)
	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the parent was contacted for %s; a capability declared here runs here", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(never.Close)
	pu.Conf.ParentURL = never.URL
	pu.Caps = declares(map[string]struct {
		stack string
		decl  *capdecl.Decl
	}{
		"mail.send":   {"svc", &capdecl.Decl{Entry: 7000}},
		"slow.thing":  {"slow", &capdecl.Decl{Timeout: 150}},
		"quiet.thing": {"quiet", &capdecl.Decl{}},
		"outer.thing": {"outer", &capdecl.Decl{}},
	})
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme', 'acme', 'Acme', '')`); err != nil {
		t.Fatal(err)
	}
	return pu, WithTenant(WithRunGrant(context.Background(), "tok-from-a-node"), "acme")
}

// seedAcmeOp seeds a rule for tenant acme.
func seedAcmeOp(t *testing.T, pu *Unit, stack string, scope int, name, txcl string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', 'tnt_acme')`,
		stack, scope, name, txcl); err != nil {
		t.Fatalf("seed %s/%d: %v", stack, scope, err)
	}
}

func runCap(t *testing.T, pu *Unit, ctx context.Context, stage string) string {
	t.Helper()
	got, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"}}`, stage)
	if err != nil || len(got) != 1 {
		t.Fatalf("Run(%s): %v %+v", stage, err, got)
	}
	return got[0].Raw
}

// The declaring stack runs at its declared entry, sees the capability
// envelope, and answers in cap://'s shape. The parent is not involved, even
// though one is configured and the request carries a run grant: a name
// declared here is answered here.
func TestExecCapDeclaredHereRunsHere(t *testing.T) {
	pu, ctx := capUnit(t)
	seedAcmeOp(t, pu, "svc", 6000, "before", `EMIT ._cap.output.leaked = true`)
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @src == "cap" AND @cap.name == "mail.send" `+
		`EMIT ._cap.output = &object("mid", "m1", "to", @cap.input.to, "stack", @stack, `+
		`"caller", @cap.caller.stack, "depth", @cap.depth, "impl", @cap.impl.to, "tenant", @cap.tenant)`)
	seedAcmeOp(t, pu, "svc", 7100, "after", `EMIT ._cap.output.after = true, .work = "mine"`)
	seedAcmeOp(t, pu, "c", 100, "ask", `WITH input = &object("to", "a@b.example"), into = "_sent" EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 200, "next", `EMIT .next = true`)

	final := runCap(t, pu, ctx, "c/0")
	out := gjson.Get(final, "_sent")
	if out.Get("mid").String() != "m1" || out.Get("to").String() != "a@b.example" || !out.Get("after").Bool() {
		t.Errorf("_sent = %s", out.Raw)
	}
	if out.Get("leaked").Exists() {
		t.Errorf("the declaring stack ran from its start, not its entry: %s", final)
	}
	if out.Get("stack").String() != "svc" || out.Get("caller").String() != "c" || out.Get("depth").Int() != 1 ||
		out.Get("impl").String() != "svc/7000" || out.Get("tenant").String() != "acme" {
		t.Errorf("the capability facts: %s", out.Raw)
	}
	if gjson.Get(final, "cap.name").String() != "mail.send" || gjson.Get(final, "cap.status").Int() != 200 ||
		!gjson.Get(final, "cap.ms").Exists() || gjson.Get(final, "cap.error").Exists() {
		t.Errorf("cap facts: %s", gjson.Get(final, "cap").Raw)
	}
	if gjson.Get(final, "work").Exists() || !gjson.Get(final, "next").Bool() {
		t.Errorf("the callee's working fields reached the caller, or the caller stopped: %s", final)
	}
}

// Every failure is data at cap.error, with the status the inlet would have
// answered: the stack's own error at 422, no answer at 404, the declared
// timeout at 504, a scope that cannot run inside a call at 500.
func TestExecCapDeclaredHereFailuresAreData(t *testing.T) {
	pu, ctx := capUnit(t)
	pu.Conf.CallStopGrace = "1s"
	callBlockingOp(t, pu, "block", 0)
	seedAcmeOp(t, pu, "svc", 7000, "refuse", `EMIT ._cap.error = &object("code", "no_such_card", "message", "card 9 is not yours")`)
	seedAcmeOp(t, pu, "quiet", 100, "shrug", `EMIT .nothing = true`)
	seedAcmeOp(t, pu, "slow", 100, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedAcmeOp(t, pu, "outer", 100, "cont", `EXEC "http://127.0.0.1:9/x" WITH mode = "continuable", continue_after = "1s"`)
	for i, name := range []string{"mail.send", "quiet.thing", "slow.thing", "outer.thing"} {
		seedAcmeOp(t, pu, "c", 100+i, "ask-"+name, `WHEN .which == "`+name+`" EXEC "cap://`+name+`"`)
	}
	for _, tc := range []struct {
		name, code string
		status     int64
	}{
		{"mail.send", "no_such_card", 422},
		{"quiet.thing", "txco_cap_unknown", 404},
		{"slow.thing", "txco_cap_timeout", 504},
		{"outer.thing", "txco_cap_unsupported", 500},
	} {
		got, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"},"which":"`+tc.name+`"}`, "c/0")
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %+v", tc.name, err, got)
		}
		final := got[0].Raw
		if gjson.Get(final, "cap.error.code").String() != tc.code || gjson.Get(final, "cap.error.status").Int() != tc.status {
			t.Errorf("%s: cap = %s", tc.name, gjson.Get(final, "cap").Raw)
		}
		if gjson.Get(final, "_cap").Raw != "{}" {
			t.Errorf("%s: a failed call should leave an empty output: %s", tc.name, final)
		}
		if tc.name == "mail.send" && gjson.Get(final, "cap.error.message").String() != "card 9 is not yours" {
			t.Errorf("the stack's message: %s", final)
		}
		if tc.name == "slow.thing" && !gjson.Get(final, "cap.error.stopped").Bool() {
			t.Errorf("the timed-out callee should report stopped: %s", final)
		}
	}
}

// A name nobody here declares goes up to the parent, as before; a chassis
// with no lookup at all does too.
func TestExecCapNotDeclaredHereGoesUp(t *testing.T) {
	p, srv := newParent(t)
	pu, _ := newTestUnit(t)
	pu.Conf.ParentURL = srv.URL
	pu.Conf.OpPayloadMax = 1 << 20
	pu.Caps = declares(nil)
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme', 'acme', 'Acme', '')`); err != nil {
		t.Fatal(err)
	}
	seedAcmeOp(t, pu, "c", 100, "ask", `WITH input = &object("n", 1) EXEC "cap://card.note"`)
	ctx := WithRunGrant(WithTenant(context.Background(), "acme"), "tok")
	got, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"}}`, "c/0")
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if gjson.Get(got[0].Raw, "_cap.seq").Int() != 7 {
		t.Errorf("the parent's answer: %s", got[0].Raw)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.path != "/v1/cap/card.note" {
		t.Errorf("the parent was asked at %q", p.path)
	}
}

// A capability may call a capability: nested frames, depth counted.
func TestExecCapDeclaredHereNests(t *testing.T) {
	pu, ctx := capUnit(t)
	seedAcmeOp(t, pu, "svc", 7000, "send", `WITH into = "_q" EXEC "cap://quiet.thing"`)
	seedAcmeOp(t, pu, "svc", 7100, "answer", `EMIT ._cap.output = &object("inner", ._q.d, "depth", @cap.depth)`)
	seedAcmeOp(t, pu, "quiet", 100, "deep", `EMIT ._cap.output.d = @cap.depth`)
	seedAcmeOp(t, pu, "c", 100, "ask", `EXEC "cap://mail.send"`)
	final := runCap(t, pu, ctx, "c/0")
	if gjson.Get(final, "_cap.inner").Int() != 2 || gjson.Get(final, "_cap.depth").Int() != 1 {
		t.Errorf("nested capabilities: %s", final)
	}
}

// A capability's run under the engine ends when the caller's request does.
func TestExecCapDeclaredHereCannotOutliveTheCaller(t *testing.T) {
	pu, ctx := capUnit(t)
	ended := callBlockingOp(t, pu, "block", 0)
	seedAcmeOp(t, pu, "svc", 7000, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedAcmeOp(t, pu, "c", 100, "ask", `EXEC "cap://mail.send" WITH timeout = 10000`)
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"}}`, "c/0")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("Run: %v after %s", err, time.Since(start))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, _ := ended.Load().(string); got == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the callee's op ended %q, want cancelled", ended.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
