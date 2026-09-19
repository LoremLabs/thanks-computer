package processor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// whoami answers with the stack the chassis says is calling: {"seen":{"<stack>":true}}.
func whoami(ctx context.Context, _ string, _, _ []byte) (event.Payload, error) {
	b, _ := json.Marshal(map[string]any{"seen": map[string]bool{StackScope(ctx): true}})
	return event.Payload{Raw: string(b), Type: event.JSON}, nil
}

// TestStackScopeComesFromTheDeployedRule — the identity ops decide who owns a
// principal from StackScope(ctx). It must be the stack of the RULE that is
// dispatching (op.Stack), per op, and nothing an envelope says: `_txc.op` is
// an envelope field, and a request crosses stacks.
func TestStackScopeComesFromTheDeployedRule(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Handle([]byte("txco://whoami"), event.OpsHandlerFunc(whoami))
	seed := func(stack string, scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			stack, scope, name, rule); err != nil {
			t.Fatalf("seed op: %v", err)
		}
	}
	run := func(stage, envelope string) string {
		t.Helper()
		resCh := make(chan event.Payload, 1)
		if err := pu.Run(context.Background(), envelope, stage, resCh); err != nil {
			t.Fatalf("Run: %v", err)
		}
		select {
		case payload := <-resCh:
			return payload.Raw
		default:
			t.Fatal("no response received")
			return ""
		}
	}
	seen := func(out string) []string {
		var stacks []string
		gjson.Get(out, "seen").ForEach(func(k, _ gjson.Result) bool { stacks = append(stacks, k.String()); return true })
		return stacks
	}

	// A canary slot's own rule reports the slot; an envelope claiming to be
	// another stack's op changes nothing.
	seed("web/canary", 0, "who", `WHEN .x == 1 EXEC "txco://whoami"`)
	out := run("web/canary/0", `{"x":1,"_txc":{"op":"evil/who","stack":"evil"}}`)
	if got := seen(out); len(got) != 1 || got[0] != "web/canary" {
		t.Errorf("canary rule saw %v: %s", got, out)
	}

	// A slot with no rule of its own at this scope inherits its base stack's
	// rule — and that rule IS the base stack's.
	seed("shop", 0, "who", `WHEN .x == 1 EXEC "txco://whoami"`)
	out = run("shop/canary/0", `{"x":1}`)
	if got := seen(out); len(got) != 1 || got[0] != "shop" {
		t.Errorf("inherited rule saw %v: %s", got, out)
	}

	// It is per op, not pinned for the request: after a cross-stack goto the
	// next stack's rules report their own stack.
	seed("first", 0, "who", `WHEN .x == 1 EXEC "txco://whoami" EMIT @goto = "second/0"`)
	seed("second", 0, "who", `WHEN .x == 1 EXEC "txco://whoami"`)
	out = run("first/0", `{"x":1}`)
	if !gjson.Get(out, `seen.first`).Bool() || !gjson.Get(out, `seen.second`).Bool() {
		t.Errorf("goto across stacks saw %v: %s", seen(out), out)
	}

	if got := StackScope(context.Background()); got != "" {
		t.Errorf("unpinned StackScope = %q", got)
	}
}

// TestPrincipalComesOnlyFromAVerifiedLogin — `_txc.principal` is the
// stack's read-only copy of the principal a head pinned on its dispatch
// context. With no pin, whatever the request brought is removed; with one,
// the copy is the pin, whatever the request said; and no rule can write it.
func TestPrincipalComesOnlyFromAVerifiedLogin(t *testing.T) {
	pu, _ := newTestUnit(t)
	seed := func(stack, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, 0, 'r', ?, '', '')`,
			stack, rule); err != nil {
			t.Fatalf("seed op: %v", err)
		}
	}
	run := func(ctx context.Context, stage, envelope string) string {
		t.Helper()
		resCh := make(chan event.Payload, 1)
		if err := pu.Run(ctx, envelope, stage, resCh); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return (<-resCh).Raw
	}
	seed("who", `WHEN .x == 1 EMIT .saw = @principal.id, .kind = @principal.kind, .cred = @principal.credential`)
	seed("forge", `WHEN .x == 1 EMIT @principal.id = "user:usr_forged1", @principal = "pony:forged"`)
	const smuggled = `{"x":1,"_txc":{"principal":{"id":"user:usr_smuggled","kind":"user"}}}`

	// No head signed anyone in: the smuggled principal is gone.
	out := run(context.Background(), "who/0", smuggled)
	if gjson.Get(out, "_txc.principal").Exists() || gjson.Get(out, "saw").String() != "" {
		t.Errorf("unpinned request kept a principal: %s", out)
	}

	pony, _ := authn.ParsePrincipal("pony:paris")
	signedIn := authn.WithAuthenticated(context.Background(), authn.Authenticated{Principal: pony, Credential: "crd_1"})
	out = run(signedIn, "who/0", smuggled)
	if gjson.Get(out, "saw").String() != "pony:paris" || gjson.Get(out, "kind").String() != "pony" || gjson.Get(out, "cred").String() != "crd_1" {
		t.Errorf("pinned request: %s", out)
	}

	out = run(signedIn, "forge/0", `{"x":1}`)
	if got := gjson.Get(out, "_txc.principal.id").String(); got != "pony:paris" {
		t.Errorf("a rule rewrote the principal to %q: %s", got, out)
	}

	if got := PrincipalScope(signedIn); got != "pony:paris" {
		t.Errorf("PrincipalScope = %q", got)
	}
	if got := PrincipalScope(context.Background()); got != "" {
		t.Errorf("unpinned PrincipalScope = %q", got)
	}
	// A resume re-pins from the chassis-stamped scope envelope.
	if a, ok := authn.AuthenticatedFrom(withPrincipalFrom(context.Background(), out)); !ok || a.Principal != pony || a.Credential != "crd_1" {
		t.Errorf("re-pin from the envelope = %+v, %v", a, ok)
	}
	if _, ok := authn.AuthenticatedFrom(withPrincipalFrom(context.Background(), `{"_txc":{"principal":{"id":"not a principal"}}}`)); ok {
		t.Error("re-pinned a malformed principal")
	}
}
