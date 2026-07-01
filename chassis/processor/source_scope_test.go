package processor

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestSourceScopePinnedAtFirstRun locks in the security property behind
// txco://relay's provenance gate: the originating inlet is pinned in trusted
// context at the first Run (from the ingress-stamped `_txc.src`), so a rule
// that rewrites `_txc.src` mid-pipeline via `SET @src` CANNOT change what a
// bundled op sees through SourceScope(ctx). A privileged op must read the
// pinned value, never the mutable envelope field.
func TestSourceScopePinnedAtFirstRun(t *testing.T) {
	pu, _ := newTestUnit(t)

	var seen atomic.Value // string
	seen.Store("")
	pu.Handle([]byte("txco://capturesrc"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			seen.Store(SourceScope(ctx))
			return event.Payload{Raw: "{}", Type: event.JSON}, nil
		}))

	// The rule forges `@src = "http"` before dispatching. The pin (captured at
	// first Run from `_txc.src="lmtp"`) must ignore it.
	rule := `WHEN .x == 1 SET @src = "http" EXEC "txco://capturesrc"`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"srcpin", 0, "hello", rule,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"x":1,"_txc":{"src":"lmtp"}}`, "srcpin/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-resCh

	if got := seen.Load().(string); got != "lmtp" {
		t.Errorf("SourceScope = %q, want %q — a mid-pipeline SET @src must not re-pin the source", got, "lmtp")
	}
}

// TestSourceScopeEmptyWhenUnstamped: an untenanted/system request with no
// `_txc.src` yields an empty scope (privileged ops then refuse — fail closed).
func TestSourceScopeEmptyWhenUnstamped(t *testing.T) {
	if got := sourceScope(context.Background()); got != "" {
		t.Errorf("sourceScope(bare ctx) = %q, want empty", got)
	}
	ctx := WithSource(context.Background(), "lmtp")
	if got := SourceScope(ctx); got != "lmtp" {
		t.Errorf("SourceScope(WithSource lmtp) = %q, want lmtp", got)
	}
}
