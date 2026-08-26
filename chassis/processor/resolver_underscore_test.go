package processor

import (
	"context"
	"testing"
)

// `_` is a SQL LIKE single-character wildcard, and every ops lookup matches
// the stack with LIKE. Stack names legitimately contain `_`: the channel
// convention (`_mail`, `_cron`, `_llm`, `_room`, `_inspect`, `_scheduled`,
// and the nested `<stack>/_mail`) plus anything a tenant chooses, since
// opname.seg permits it — while its own comment claims SQL LIKE wildcards
// are excluded (only `%` actually is). Unescaped, a lookup for `_mail`
// also returns `email`'s rules, merged at the same scope.

// underscoreProbe runs one stage through BOTH resolution paths — the
// in-memory ops index and the SQL fallback — because they carry separate
// copies of the LIKE clause and must not disagree.
func underscoreProbe(t *testing.T, pu *Unit, sqlHandle any, tenant, stage string, want []string) {
	t.Helper()
	paths := []struct {
		name string
		ctx  context.Context
	}{
		{"index", WithTenant(context.Background(), tenant)},
		{"sql", context.WithValue(WithTenant(context.Background(), tenant), ctxKeyOpstackSnap, sqlHandle)},
	}
	for _, p := range paths {
		ops, err := pu.OpsForStage(p.ctx, stage)
		if err != nil {
			t.Fatalf("%s path %s: %v", p.name, stage, err)
		}
		got := make([]string, 0, len(ops))
		for _, op := range ops {
			got = append(got, op.Stack)
		}
		if len(got) != len(want) {
			t.Errorf("%s path %s: got %d ops %v, want %d %v", p.name, stage, len(got), got, len(want), want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s path %s: op %d from stack %q, want %q (full: %v)", p.name, stage, i, got[i], want[i], got)
			}
		}
	}
}

// TestUnderscoreInStackNameIsNotALikeWildcard is the bug: a request routed
// to the `_mail` channel must run ONLY the mail rules, never the rules of a
// same-length stack that happens to end in "mail".
func TestUnderscoreInStackNameIsNotALikeWildcard(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")

	// `_mail` (5 chars) LIKE-matches `email`; `pony/_mail` matches `pony/email`.
	seedIdxOp(t, pu.Dbc.Db, "t-a", "_mail", 100, "channel", `EXEC "txco://mail"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "email", 100, "unrelated", `EXEC "txco://web"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "pony/_mail", 100, "channel", `EXEC "txco://mail"`)
	seedIdxOp(t, pu.Dbc.Db, "t-a", "pony/email", 100, "unrelated", `EXEC "txco://web"`)

	underscoreProbe(t, pu, sqlHandle, "alpha", "_mail/100", []string{"_mail"})
	underscoreProbe(t, pu, sqlHandle, "alpha", "email/100", []string{"email"})
	underscoreProbe(t, pu, sqlHandle, "alpha", "pony/_mail/100", []string{"pony/_mail"})
	underscoreProbe(t, pu, sqlHandle, "alpha", "pony/email/100", []string{"pony/email"})
}

// TestUnderscoreStackUsesOpsIndex: `strings.ContainsAny(stack, "%_")` treated
// every `_`-bearing stack as a LIKE pattern and sent it down the SQL path,
// so the mail/cron/llm channels re-parsed every rule on every request —
// exactly the regression opsindex.go exists to remove.
func TestUnderscoreStackUsesOpsIndex(t *testing.T) {
	pu, _ := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "pony/_mail", 100, "channel", `EXEC "txco://mail"`)

	ctx := WithTenant(context.Background(), "alpha")
	ops, err := pu.OpsForStage(ctx, "pony/_mail/100")
	if err != nil {
		t.Fatalf("OpsForStage: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d ops, want 1", len(ops))
	}
	if pu.opsIdx.Load() == nil {
		t.Fatal("ops index was never built for a `_`-bearing stack — it took the SQL re-parse path")
	}
	if ops[0].Resonator == nil {
		t.Error("op came back unparsed — the index path pre-parses, the SQL path does not")
	}
}

// TestUnderscoreStackStillPeels: an ordinary segment containing `_`
// (`publications/my_book`) is a normal child and must inherit its parent's
// rules. The wildcard gate disabled the peel for it.
func TestUnderscoreStackStillPeels(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "publications", 100, "shared", `EXEC "txco://drip"`)

	underscoreProbe(t, pu, sqlHandle, "alpha", "publications/my_book/100", []string{"publications"})
}

// TestPeelStopsAtChannelBoundary: `<stack>/_mail` is a CHANNEL, not a
// specialization. A mail request that finds no rules must return empty
// rather than inherit the HTTP stack's rules and run them on an inbound
// message. The `_`-wildcard gate blocked this by accident; the fix must
// keep it blocked on purpose.
func TestPeelStopsAtChannelBoundary(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "www", 100, "http-only", `EXEC "txco://web"`)

	underscoreProbe(t, pu, sqlHandle, "alpha", "www/_mail/100", nil)
	underscoreProbe(t, pu, sqlHandle, "alpha", "www/_cron/100", nil)
	// The ordinary child still inherits — the boundary is the `_` prefix,
	// not the presence of a slash.
	underscoreProbe(t, pu, sqlHandle, "alpha", "www/canary/100", []string{"www"})
}

// TestPercentRemainsAWildcard guards the escape: `boot/%` is the real
// ingress-miss fallthrough pattern (config.IngressMissAction) and must keep
// matching across stacks.
func TestPercentRemainsAWildcard(t *testing.T) {
	pu, sqlHandle := newFileBackedUnit(t)
	seedIdxTenant(t, pu.Dbc.Db, "t-a", "alpha")
	seedIdxOp(t, pu.Dbc.Db, "t-a", "boot/example", 0, "catchall", `EXEC "txco://example"`)

	underscoreProbe(t, pu, sqlHandle, "alpha", "boot/%/0", []string{"boot/example"})
}
