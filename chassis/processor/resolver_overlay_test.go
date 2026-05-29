package processor

import (
	"context"
	"testing"
)

// TestOpsForStageOverlayFallback covers the prefix-fallback resolver: a
// request at `website/canary/N` finds canary's row when one exists and
// falls back to `website` when canary has nothing at that scope. Sparse
// override trees are valid and the runtime fills the gaps.
func TestOpsForStageOverlayFallback(t *testing.T) {
	pu, _ := newTestUnit(t)

	seed := func(stack string, scope int, txcl string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`, stack, scope, txcl); err != nil {
			t.Fatalf("seed (%s,%d): %v", stack, scope, err)
		}
	}

	seed("website", 100, `EXEC "txco://web-100"`)
	seed("website", 500, `EXEC "txco://web-500"`)
	seed("website/canary", 100, `EXEC "txco://canary-100"`)

	cases := []struct {
		name      string
		stage     string
		wantStack string
		wantScope int
	}{
		{"canary override at 100", "website/canary/100", "website/canary", 100},
		{"canary fallback at 500", "website/canary/500", "website", 500},
		{"deep canary fallback", "website/canary/eu/500", "website", 500},
		{"plain website at 100", "website/100", "website", 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops, err := pu.OpsForStage(context.Background(), tc.stage)
			if err != nil {
				t.Fatalf("OpsForStage(%s): %v", tc.stage, err)
			}
			if len(ops) != 1 {
				t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
			}
			if ops[0].Stack != tc.wantStack || ops[0].Scope != tc.wantScope {
				t.Errorf("got (%s,%d), want (%s,%d)", ops[0].Stack, ops[0].Scope, tc.wantStack, tc.wantScope)
			}
		})
	}
}

// TestOpsForStageOverlayNoMatch checks the genuine miss case: a request that
// can't resolve at any prefix returns no ops without erroring. The pipeline
// terminates naturally when the result is empty.
func TestOpsForStageOverlayNoMatch(t *testing.T) {
	pu, _ := newTestUnit(t)

	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"website", 100, `EXEC "txco://web-100"`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ops, err := pu.OpsForStage(context.Background(), "support/triage/100")
	if err != nil {
		t.Fatalf("OpsForStage: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("got %d ops, want 0: %+v", len(ops), ops)
	}
}

// TestOpsForStageWildcardNoFallback verifies that a wildcard stack (the boot
// pattern `boot/%`) does not trigger prefix-fallback. The wildcard already
// matches across stacks at one level; peeling it would produce surprising
// hits on totally unrelated rules.
func TestOpsForStageWildcardNoFallback(t *testing.T) {
	pu, _ := newTestUnit(t)

	// Seed a non-boot rule. If wildcard fallback erroneously peeled `boot/%`
	// down to `boot` and then `""`, the empty stack pattern combined with
	// LIKE could still pick this up as a stray match. Either way, the
	// wildcard test asserts the wildcard query itself returns its own rows
	// (or nothing), not anything from a parent peel.
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/example", 0, `EXEC "txco://example"`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ops, err := pu.OpsForStage(context.Background(), "boot/%/0")
	if err != nil {
		t.Fatalf("OpsForStage: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1: %+v", len(ops), ops)
	}
	if ops[0].Stack != "boot/example" {
		t.Errorf("got stack %q, want boot/example", ops[0].Stack)
	}
}

func TestStackParent(t *testing.T) {
	cases := []struct {
		in     string
		out    string
		wantOk bool
	}{
		{"website/canary/eu", "website/canary", true},
		{"website/canary", "website", true},
		{"website", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := stackParent(tc.in)
		if got != tc.out || ok != tc.wantOk {
			t.Errorf("stackParent(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.out, tc.wantOk)
		}
	}
}
