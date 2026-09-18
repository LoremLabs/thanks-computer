package processor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

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
