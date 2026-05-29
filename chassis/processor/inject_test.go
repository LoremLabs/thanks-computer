package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestRunInjectsTxcOpAndStepOnHandlerDispatch checks the HTTP transport
// of the dispatch boundary. A rule at (stack=svc, scope=100, name=handler)
// fires and POSTs an envelope to a test server. The chassis must have
// stamped _txc.op=svc/handler and _txc.step=100 onto the body before
// the handler ever sees it.
func TestRunInjectsTxcOpAndStepOnHandlerDispatch(t *testing.T) {
	var receivedBody []byte
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 100, "handler", `EXEC "`+srv.URL+`"`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("handler hits = %d, want 1", got)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.op").String(); got != "svc/handler" {
		t.Errorf("_txc.op = %q, want %q (body=%s)", got, "svc/handler", receivedBody)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.step").Int(); got != 100 {
		t.Errorf("_txc.step = %d, want 100 (body=%s)", got, receivedBody)
	}
}

// TestRunInjectsTxcOpAndStepForTxcoBuiltin is the transport-agnostic
// lock-in. A rule at (stack=svc, scope=42, name=local) dispatches to a
// txco://capture builtin (chassis-resident, no HTTP). The injection
// must still happen — the abstraction is "envelope is being handed to a
// handler," not "an HTTP request is being built." If anyone refactors
// the injection to live inside ExecHTTP, this test catches it.
func TestRunInjectsTxcOpAndStepForTxcoBuiltin(t *testing.T) {
	var (
		mu       sync.Mutex
		captured []byte
		hits     int32
	)

	pu, _ := newTestUnit(t)
	pu.Handle([]byte("txco://capture"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			atomic.AddInt32(&hits, 1)
			mu.Lock()
			captured = append([]byte(nil), in...)
			mu.Unlock()
			return event.Payload{Raw: "{}", Type: event.JSON}, nil
		}))

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 42, "local", `EXEC "txco://capture"`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "svc/42", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("txco builtin hits = %d, want 1", got)
	}
	mu.Lock()
	body := captured
	mu.Unlock()
	if got := gjson.GetBytes(body, "_txc.op").String(); got != "svc/local" {
		t.Errorf("_txc.op = %q, want %q (body=%s) — injection is not transport-agnostic", got, "svc/local", body)
	}
	if got := gjson.GetBytes(body, "_txc.step").Int(); got != 42 {
		t.Errorf("_txc.step = %d, want 42 (body=%s)", got, body)
	}
}

// TestRunDoesNotInjectFromStageJumper ensures the jumper rule's identity
// does not leak into the downstream handler's envelope. The handler
// rule fires after the stage jump, so its identity is what the handler
// receives — not the jumper's.
func TestRunDoesNotInjectFromStageJumper(t *testing.T) {
	var receivedBody []byte
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	// Router stage-jumps to svc/100. Router itself has no handler;
	// the stage jump synthesizes a goto.
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"router", 0, "go", `EXEC "svc/100"`,
	); err != nil {
		t.Fatalf("seed router: %v", err)
	}
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 100, "handler", `EXEC "`+srv.URL+`"`,
	); err != nil {
		t.Fatalf("seed handler: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "router/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("handler hits = %d, want 1", got)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.op").String(); got != "svc/handler" {
		t.Errorf("_txc.op = %q, want %q (jumper's identity should not leak; body=%s)",
			got, "svc/handler", receivedBody)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.step").Int(); got != 100 {
		t.Errorf("_txc.step = %d, want 100 (body=%s)", got, receivedBody)
	}
}

// TestRunOverwritesIncomingTxcOpAndStep locks in overwrite-don't-merge.
// The starting envelope carries stale _txc.op/_txc.step from some prior
// stage. The rule firing now must replace them, not merge with them.
func TestRunOverwritesIncomingTxcOpAndStep(t *testing.T) {
	var receivedBody []byte
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 200, "handler", `EXEC "`+srv.URL+`"`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stale := `{"_txc":{"op":"stale/value","step":99}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), stale, "svc/200", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("handler hits = %d, want 1", got)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.op").String(); got != "svc/handler" {
		t.Errorf("_txc.op = %q, want %q (overwrite failed; body=%s)", got, "svc/handler", receivedBody)
	}
	if got := gjson.GetBytes(receivedBody, "_txc.step").Int(); got != 200 {
		t.Errorf("_txc.step = %d, want 200 (overwrite failed; body=%s)", got, receivedBody)
	}
}
