package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestRunTxcDeleteRemovesTreesAfterConsumption pins the `_txc.delete`
// envelope directive (authored as `EMIT @delete = [...]`): a later-scope
// op can read a value, and an even-later op deletes the source tree from
// the merged envelope so it never reaches the response — while the
// derived value survives. This is the "call two APIs, keep only the
// summary" pattern. Delete is general (mutates the merged envelope, not a
// web-only projection) and the directive itself is stripped so it neither
// leaks into the response nor re-fires.
func TestRunTxcDeleteRemovesTreesAfterConsumption(t *testing.T) {
	pu, _ := newTestUnit(t)
	seed := func(scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"app", scope, name, rule); err != nil {
			t.Fatalf("seed app/%d/%s: %v", scope, name, err)
		}
	}
	// scope 0: two values land on the envelope (stand-ins for two API
	// responses nested under their own keys).
	seed(0, "london", `EMIT .london = "L"`)
	seed(0, "tokyo", `EMIT .tokyo = "T"`)
	// scope 100: a consumer reads BOTH before they're deleted.
	seed(100, "summary", `EMIT .summary = &concat(.london, .tokyo)`)
	// scope 200: trim the raw trees, keeping only the derived summary.
	seed(200, "trim", `EMIT @delete = ["london", "tokyo"]`)

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"_txc":{"src":"http"}}`, "app/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		// The consumer ran BEFORE the delete, so the derived value is intact.
		if got := gjson.Get(payload.Raw, "summary").String(); got != "LT" {
			t.Errorf("summary = %q, want %q (body=%s)", got, "LT", payload.Raw)
		}
		// The source trees were deleted from the merged envelope.
		if gjson.Get(payload.Raw, "london").Exists() {
			t.Errorf("london should be deleted; body=%s", payload.Raw)
		}
		if gjson.Get(payload.Raw, "tokyo").Exists() {
			t.Errorf("tokyo should be deleted; body=%s", payload.Raw)
		}
		// The directive must not leak into the response.
		if gjson.Get(payload.Raw, "_txc.delete").Exists() {
			t.Errorf("_txc.delete should be stripped after consumption; body=%s", payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunTxcDeleteNestedPathAndSingleString covers two shapes: a dotted
// path deletes just a nested key (the parent survives), and a single
// string (not an array) is accepted as one path.
func TestRunTxcDeleteNestedPathAndSingleString(t *testing.T) {
	pu, _ := newTestUnit(t)
	seed := func(scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"app", scope, name, rule); err != nil {
			t.Fatalf("seed app/%d/%s: %v", scope, name, err)
		}
	}
	seed(0, "seed", `EMIT .keep = "yes", .city.name = "London", .city.secret = "hush"`)
	// Single-string form (not an array) + a dotted nested path.
	seed(100, "trim", `EMIT @delete = "city.secret"`)

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"_txc":{"src":"http"}}`, "app/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "city.name").String(); got != "London" {
			t.Errorf("city.name should survive; got %q (body=%s)", got, payload.Raw)
		}
		if gjson.Get(payload.Raw, "city.secret").Exists() {
			t.Errorf("city.secret should be deleted; body=%s", payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "keep").String(); got != "yes" {
			t.Errorf("keep should survive; got %q (body=%s)", got, payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}
