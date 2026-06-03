package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestSpentRouteProposalStripped pins the cleanup of the inert _txc.route
// proposal. In the real boot pipeline detect-tenant@0 writes _txc.route.*
// and txco://route@100 promotes it into _txc.{goto,tenant,…}; op output is
// merged (which can only add), so the spent proposal is removed where the
// other consumed control fields are removed — advanceAfterScope's halt/goto
// strip. Here we inject the proposal on the inbound envelope (standing in
// for detect@0) and consume the scope with a goto, then a halt — both strip
// triggers — and assert _txc.route does not survive while ordinary data the
// rule sets alongside it does.
func TestSpentRouteProposalStripped(t *testing.T) {
	// goto path — mirrors route@100, where a stage jump consumes the proposal.
	t.Run("goto", func(t *testing.T) {
		pu, _ := newTestUnit(t)
		seed := func(scope int, name, rule string) {
			t.Helper()
			if _, err := pu.Dbc.Db.Exec(
				`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
				"app", scope, name, rule); err != nil {
				t.Fatalf("seed app/%d/%s: %v", scope, name, err)
			}
		}
		seed(0, "jump", `EMIT @goto = "app/100", .kept = "yes"`)
		seed(100, "land", `EMIT .landed = "ok"`)

		in := `{"_txc":{"src":"http","route":{"tenant":"t1","stack":"app","to":"app/100"}}}`
		resCh := make(chan event.Payload, 1)
		if err := pu.Run(context.Background(), in, "app/0", resCh); err != nil {
			t.Fatalf("Run: %v", err)
		}
		select {
		case payload := <-resCh:
			if gjson.Get(payload.Raw, "_txc.route").Exists() {
				t.Errorf("_txc.route should be stripped once its goto is consumed; body=%s", payload.Raw)
			}
			if got := gjson.Get(payload.Raw, "kept").String(); got != "yes" {
				t.Errorf("ordinary data set alongside the goto should survive; kept=%q body=%s", got, payload.Raw)
			}
			if got := gjson.Get(payload.Raw, "landed").String(); got != "ok" {
				t.Errorf("goto did not land at app/100; body=%s", payload.Raw)
			}
		default:
			t.Error("expected a payload on resCh")
		}
	})

	// halt path — mirrors static@50 terminating the request on a file hit.
	t.Run("halt", func(t *testing.T) {
		pu, _ := newTestUnit(t)
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"app", 0, "stop", `EMIT @halt = true, .kept = "yes"`); err != nil {
			t.Fatalf("seed: %v", err)
		}
		in := `{"_txc":{"src":"http","route":{"stack":"app","to":"app/100"}}}`
		resCh := make(chan event.Payload, 1)
		if err := pu.Run(context.Background(), in, "app/0", resCh); err != nil {
			t.Fatalf("Run: %v", err)
		}
		select {
		case payload := <-resCh:
			if gjson.Get(payload.Raw, "_txc.route").Exists() {
				t.Errorf("_txc.route should be stripped on halt; body=%s", payload.Raw)
			}
			if got := gjson.Get(payload.Raw, "kept").String(); got != "yes" {
				t.Errorf("ordinary data should survive; kept=%q body=%s", got, payload.Raw)
			}
		default:
			t.Error("expected a payload on resCh")
		}
	})
}
