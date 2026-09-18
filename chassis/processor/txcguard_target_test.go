package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/ops"
)

// runOneRule seeds a single rule at <stack>/0, runs the envelope through it,
// and returns the propagated envelope.
func runOneRule(t *testing.T, stack, rule, envelope string) string {
	t.Helper()
	return runNamedRule(t, stack, "rule", rule, envelope)
}

// runNamedRule is runOneRule with the op's name chosen by the caller.
func runNamedRule(t *testing.T, stack, name, rule, envelope string) string {
	t.Helper()
	pu, _ := newTestUnit(t)
	pu.Handle([]byte("txco://copy"), event.OpsHandlerFunc(ops.Copy))
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		stack, 0, name, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), envelope, stack+"/0", resCh); err != nil {
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

// TestTrustedOpCannotForgeReservedViaTarget — txco://copy runs on the TRUSTED
// transport, so its output reaches the merge without the reserved-`_txc`
// sanitizer. Its `to` is author-chosen, which made `to = "@imap.account"` a
// one-line forge of a chassis-stamped identity. The target is now checked in
// the op (txcguard.AuthorTarget), in every spelling sjson resolves to the
// reserved path.
func TestTrustedOpCannotForgeReservedViaTarget(t *testing.T) {
	const envelope = `{"x":1,"claim":"victim","_txc":{"src":"http","imap":{"account":"real@example.test"}}}`
	forges := []struct{ to, probe string }{
		{"@imap.account", "_txc.imap.account"},
		{"_txc.imap.account", "_txc.imap.account"},
		{`\\_txc.imap.account`, "_txc.imap.account"}, // txcl string escape → `\_txc.imap.account`
		{":_txc.imap.account", "_txc.imap.account"},
		{"@computed.sig_valid", "_txc.computed.sig_valid"},
		{"@principal", "_txc.principal"},
		{"@tenant", "_txc.tenant"},
	}
	for _, f := range forges {
		rule := `WHEN .x == 1 EXEC "txco://copy" WITH from = ".claim", to = "` + f.to + `"`
		got := runOneRule(t, "forge", rule, envelope)
		if v := gjson.Get(got, f.probe); v.String() == "victim" {
			t.Errorf("to=%q forged %s: %s", f.to, f.probe, got)
		}
		if acct := gjson.Get(got, "_txc.imap.account").String(); acct != "real@example.test" {
			t.Errorf("to=%q changed the stamped account to %q: %s", f.to, acct, got)
		}
	}

	// The documented uses still work: the author's own key, and the
	// author-writable response body.
	for _, to := range []string{".copied", "_txc.web.res.body", "@web.res.body"} {
		rule := `WHEN .x == 1 EXEC "txco://copy" WITH from = ".claim", to = "` + to + `"`
		got := runOneRule(t, "legit", rule, envelope)
		probe := normalizeEnvelopePath(to)
		if v := gjson.Get(got, probe).String(); v != "victim" {
			t.Errorf("to=%q: legitimate copy did not land at %s: %s", to, probe, got)
		}
	}
}

// TestDeleteGuardRefusesEscapedReservedPaths — the `_txc.delete` guard judged
// the raw string, so `:_txc.src` and `\_txc.src` (which sjson resolves to
// `_txc.src`) slipped past a check that correctly refused `@src`. Budget
// fields (`fuel_used`, `_seen`, `ttl`) were deletable the same way.
func TestDeleteGuardRefusesEscapedReservedPaths(t *testing.T) {
	const envelope = `{"x":1,"_txc":{"src":"http","web":{"req":{"url":"u","body":"Ym9keQ=="}}}}`
	for _, target := range []string{"@src", "_txc.src", ":_txc.src", `\\_txc.src`, "_txc.:src"} {
		rule := `WHEN .x == 1 EMIT @delete = ["` + target + `"]`
		got := runOneRule(t, "del", rule, envelope)
		if gjson.Get(got, "_txc.src").String() != "http" {
			t.Errorf("@delete %q removed reserved _txc.src: %s", target, got)
		}
	}

	// A delete-only fact is still deletable, in either spelling.
	for _, target := range []string{"@web.req.body", ":_txc.web.req.body"} {
		rule := `WHEN .x == 1 EMIT @delete = ["` + target + `"]`
		got := runOneRule(t, "del", rule, envelope)
		if gjson.Get(got, "_txc.web.req.body").Exists() {
			t.Errorf("@delete %q did not remove the consumed body: %s", target, got)
		}
		if gjson.Get(got, "_txc.web.req.url").String() != "u" {
			t.Errorf("@delete %q removed more than the body: %s", target, got)
		}
	}
}
