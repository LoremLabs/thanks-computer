package processor

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestRunSnapshotsOpstackForDurationOfRequest verifies the "stack
// selected at request start doesn't change mid-flight" guarantee. We
// kick off a request, then swap pu.Dbc.Db to a totally empty DB while
// the request is between stages, and confirm the later stage still
// uses the original opstack (so the response carries data the swapped
// DB couldn't possibly produce).
//
// Implementation: stage 0 jumps to stage 100 via _txc.goto. While the
// httptest server is handling the stage-0 request, we swap dbc.Db to
// an empty in-memory DB. The chassis advances to stage 100, fires its
// rule, and returns. Without snapshotting, the swapped DB would have
// no rules at stage 100 and the response would be the unchanged
// envelope. With snapshotting, stage 100's rule fires from the
// captured pre-swap DB.
func TestRunSnapshotsOpstackForDurationOfRequest(t *testing.T) {
	var stage100Hits int32

	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stage100Hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stage100":"ran"}`))
	}))
	t.Cleanup(srv100.Close)

	// Stage 0 sleeps long enough for us to swap the DB out from under
	// the request, then sets _txc.goto to advance to stage 100.
	swapBlocker := make(chan struct{})
	swapDone := make(chan struct{})
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Signal that the request has reached stage 0's op handler.
		close(swapBlocker)
		// Wait until the test has swapped pu.Dbc.Db.
		<-swapDone
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_txc":{"goto":"snapped/100"}}`))
	}))
	t.Cleanup(srv0.Close)

	pu, _ := newTestUnit(t)

	seed := func(stack string, scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			stack, scope, name, rule); err != nil {
			t.Fatalf("seed %s/%d/%s: %v", stack, scope, name, err)
		}
	}
	seed("snapped", 0, "router", `EXEC "`+srv0.URL+`/route"`)
	seed("snapped", 100, "handler", `EXEC "`+srv100.URL+`/handle"`)

	// Drive Run in a goroutine so the test can manipulate state mid-flight.
	resCh := make(chan event.Payload, 1)
	done := make(chan error, 1)
	go func() {
		done <- pu.Run(context.Background(), `{}`, "snapped/0", resCh)
	}()

	// Wait until stage 0's handler reports it's mid-request, then swap
	// the chassis's view of the DB to an empty one. With snapshot
	// semantics the request shouldn't notice — it's holding the
	// pre-swap DB pointer.
	<-swapBlocker
	emptyDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open empty: %v", err)
	}
	emptyDB.SetMaxOpenConns(1)
	if _, err := emptyDB.Exec(`CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, UNIQUE(stack, scope, txcl));`); err != nil {
		t.Fatalf("create ops on empty: %v", err)
	}
	t.Cleanup(func() { _ = emptyDB.Close() })

	pu.Dbc.Mu.Lock()
	pu.Dbc.Db = emptyDB
	pu.Dbc.Mu.Unlock()

	// Release stage 0's handler so the request can advance to stage 100.
	close(swapDone)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s")
	}

	if got := atomic.LoadInt32(&stage100Hits); got != 1 {
		t.Fatalf("stage 100 hits = %d, want 1 (snapshot failed: post-swap DB lookup missed the rule)", got)
	}
	select {
	case payload := <-resCh:
		if v := gjson.Get(payload.Raw, "stage100").String(); v != "ran" {
			t.Errorf("response missing stage100 field: %s", payload.Raw)
		}
	default:
		t.Error("expected a response on resCh")
	}
}
