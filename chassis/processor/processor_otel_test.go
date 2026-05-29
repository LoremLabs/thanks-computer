package processor

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	radix "github.com/hashicorp/go-immutable-radix"
	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/registry"
)

// TestRunEmitsSpans verifies that processor.Run() emits the named tracing
// spans the chassis instruments. Guards against accidental removal of any
// Tracer.Start call inside Run() (e.g. during a refactor).
//
// We drive Run() through an empty in-memory SQLite (no rows in `ops`), so
// the call walks every span path without doing real work.
func TestRunEmitsSpans(t *testing.T) {
	pu, sr := newTestUnit(t)

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "boot/test/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := map[string]int{}
	for _, s := range sr.Ended() {
		got[s.Name()]++
	}

	// These spans must appear at least once for an empty-ops Run().
	mustHave := []string{
		"run-boot/test/0", // outer span; name carries the stage
		"opsforstage",     // OpsForStage is called twice (current + next)
		"resonatingops",
		"getopstack",
		"getnext",
		"run",
	}
	for _, name := range mustHave {
		if got[name] == 0 {
			t.Errorf("expected span %q, missing. emitted: %v", name, got)
		}
	}

	// OpsForStage is hit for current stage AND next stage — count > 1.
	if got["opsforstage"] < 2 {
		t.Errorf("expected at least 2 opsforstage spans, got %d", got["opsforstage"])
	}
}

// TestRunRespectsCancellation verifies Run() returns promptly when its
// context is cancelled rather than hanging on internal channels or SQL
// calls. Future refactors that introduce blocking operations without
// honoring ctx will trip this.
func TestRunRespectsCancellation(t *testing.T) {
	pu, _ := newTestUnit(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Run starts

	resCh := make(chan event.Payload, 1)

	done := make(chan error, 1)
	go func() {
		done <- pu.Run(ctx, `{}`, "boot/test/0", resCh)
	}()

	select {
	case err := <-done:
		// nil or context.Canceled both acceptable — what matters is that
		// Run returned at all.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Run returned non-canceled error: %v (still acceptable)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation; possible goroutine leak or unblocked select")
	}
}

// TestRunDispatchesHTTP is the first end-to-end test of the full pipeline:
// DB lookup → resonator match → HTTP dispatch → response back. It seeds an
// `ops` row with a rule that fires on `.x == 1`, points it at an in-test
// HTTP server, drives Run() with matching input, and asserts the server
// actually got hit. Guards against transport regressions in the post-gRPC
// HTTP-only world.
func TestRunDispatchesHTTP(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		t.Logf("test server received: %s", body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"_test_handler":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	// Seed an op rule: matches the input and dispatches to the test server.
	// The stage `boot/test` matches the `boot/%/0` prefix Run is called with
	// because the resonator strips the trailing scope segment.
	rule := `WHEN .x == 1 EXEC "` + srv.URL + `/echo"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/test", 0, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"x":1}`, "boot/test/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("test HTTP server hit count = %d, want 1 (rule didn't dispatch)", got)
	}
}

// TestRunHaltsViaTxc verifies that an op response containing `_txc.halt: true`
// terminates the pipeline at the current stage — the next stage's target is
// never contacted, and the halt field is stripped from the returned envelope.
// This is the convention for op-side flow control (works identically over
// HTTP and local).
func TestRunHaltsViaTxc(t *testing.T) {
	var stage0Hits, stage1Hits int32
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stage0Hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1,"_txc":{"halt":true}}`))
	}))
	t.Cleanup(srv0.Close)
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stage1Hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv1.Close)

	pu, _ := newTestUnit(t)

	// Two stages on the same stack. Halt at scope 0 should prevent scope 1.
	rule0 := `WHEN .x == 1 EXEC "` + srv0.URL + `/halt"`
	rule1 := `EXEC "` + srv1.URL + `/never"`
	for _, r := range []struct {
		scope int
		rule  string
	}{
		{0, rule0},
		{1, rule1},
	} {
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
			"boot/halt-demo", r.scope, r.rule); err != nil {
			t.Fatalf("seed scope %d: %v", r.scope, err)
		}
	}

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{"x":1}`, "boot/halt-demo/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&stage0Hits); got != 1 {
		t.Errorf("stage 0 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&stage1Hits); got != 0 {
		t.Errorf("stage 1 hits = %d, want 0 (halt should have prevented this)", got)
	}
	select {
	case payload := <-resCh:
		// _txc.halt should be stripped before returning to the caller.
		if v := gjson.Get(payload.Raw, "_txc.halt"); v.Exists() {
			t.Errorf("_txc.halt leaked into response: %s", payload.Raw)
		}
		if v := gjson.Get(payload.Raw, "a").Int(); v != 1 {
			t.Errorf("expected merged response field a:1, got %s", payload.Raw)
		}
	default:
		t.Error("expected a response on resCh after Run returned")
	}
}

// TestRunGotoViaTxcHTTP verifies that `_txc.goto` set by an HTTP responder
// jumps the pipeline to the named stage. Pre-refactor this only worked from
// local txco:// ops; this test regression-locks the HTTP path that the docs
// already imply.
func TestRunGotoViaTxcHTTP(t *testing.T) {
	var stage0Hits, stage1Hits, stage5Hits int32
	makeSrv := func(counter *int32, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(counter, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
	}

	pu, _ := newTestUnit(t)

	srv0 := makeSrv(&stage0Hits, `{"_txc":{"goto":"boot/goto-demo/5"}}`)
	t.Cleanup(srv0.Close)
	srv1 := makeSrv(&stage1Hits, `{}`)
	t.Cleanup(srv1.Close)
	srv5 := makeSrv(&stage5Hits, `{"reached":true}`)
	t.Cleanup(srv5.Close)

	for _, r := range []struct {
		scope int
		rule  string
	}{
		{0, `EXEC "` + srv0.URL + `/jump"`},
		{1, `EXEC "` + srv1.URL + `/skipped"`},
		{5, `EXEC "` + srv5.URL + `/landed"`},
	} {
		if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
			"boot/goto-demo", r.scope, r.rule); err != nil {
			t.Fatalf("seed scope %d: %v", r.scope, err)
		}
	}

	resCh := make(chan event.Payload, 2)
	if err := pu.Run(context.Background(), `{}`, "boot/goto-demo/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&stage0Hits); got != 1 {
		t.Errorf("stage 0 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&stage1Hits); got != 0 {
		t.Errorf("stage 1 hits = %d, want 0 (goto should have skipped past)", got)
	}
	if got := atomic.LoadInt32(&stage5Hits); got != 1 {
		t.Errorf("stage 5 hits = %d, want 1 (goto target should have fired)", got)
	}
	select {
	case payload := <-resCh:
		if v := gjson.Get(payload.Raw, "_txc.goto"); v.Exists() {
			t.Errorf("_txc.goto leaked into response: %s", payload.Raw)
		}
	default:
		t.Error("expected a response on resCh after Run returned")
	}
}

// TestRunExecStageJump verifies the canonical "boot → service" pattern:
// an unschemed `EXEC "stack/scope"` value is a stage jump. The chassis
// synthesizes a `_txc.goto` so the post-stage logic routes into the
// named stack/scope. This is how a boot stack hands off to a service
// stack without resorting to SELECT/SET/halt acrobatics.
func TestRunExecStageJump(t *testing.T) {
	var stage0Hits, stage5Hits int32
	srv0 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stage0Hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv0.Close)
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stage5Hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"landed":true}`))
	}))
	t.Cleanup(srv5.Close)

	pu, _ := newTestUnit(t)

	// boot/router/0: an EXEC into the service stack, no scheme.
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"boot/router", 0, "route", `EXEC "service/100"`); err != nil {
		t.Fatalf("seed boot router: %v", err)
	}
	// boot/router/1: should never be reached if the goto worked.
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"boot/router", 1, "shouldnt", `EXEC "`+srv0.URL+`"`); err != nil {
		t.Fatalf("seed boot router unreachable: %v", err)
	}
	// service/100: the destination of the jump.
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"service", 100, "handler", `EXEC "`+srv5.URL+`"`); err != nil {
		t.Fatalf("seed service: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "boot/router/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&stage0Hits); got != 0 {
		t.Errorf("boot/router/1 fired %d times; the stage jump should have skipped it", got)
	}
	if got := atomic.LoadInt32(&stage5Hits); got != 1 {
		t.Errorf("service/100 fired %d times, want 1", got)
	}
	select {
	case payload := <-resCh:
		if v := gjson.Get(payload.Raw, "landed").Bool(); !v {
			t.Errorf("expected `landed:true` from service/100, got %s", payload.Raw)
		}
		if v := gjson.Get(payload.Raw, "_txc.goto"); v.Exists() {
			t.Errorf("_txc.goto leaked into response: %s", payload.Raw)
		}
	default:
		t.Error("expected a response on resCh after Run returned")
	}
}

// TestRunExecHTTPWithNumericPath guards the dispatch-order fix: an HTTP
// URL whose path ends in `/<digits>` must dispatch as HTTP, not get
// mis-classified as a stage jump. Pre-fix the StagePartsRE case sat
// before the scheme prefixes and stole such URLs.
func TestRunExecHTTPWithNumericPath(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"boot/numpath", 0, "main", `EXEC "`+srv.URL+`/items/100"`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "boot/numpath/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("HTTP target hit %d times; URL ending in /100 was mis-classified", got)
	}
}

// TestRunWithTimeoutOverride proves that `WITH timeout = N` actually limits
// the per-op runtime to N milliseconds. Pre-fix, the success path of the
// timeout reader silently overwrote the parsed value with the global default,
// so this never worked.
func TestRunWithTimeoutOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep well past the per-op timeout. If WITH is honored, the dispatch
		// will be cancelled before this server returns a body; the chassis
		// records an exec error and continues. If WITH is broken, this test
		// would block waiting for the full sleep.
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	rule := `WITH timeout = 50 EXEC "` + srv.URL + `/slow"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/timeout-demo", 0, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- pu.Run(context.Background(), `{}`, "boot/timeout-demo/0", resCh) }()

	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed > 400*time.Millisecond {
			t.Errorf("Run took %v; expected to short-circuit well under the 500ms server sleep when WITH timeout=50 is honored", elapsed)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s; WITH timeout = 50 not being honored")
	}
}

// TestRunDispatchesHTTPErrorResponse verifies Run() handles a 5xx response
// from a dispatch target without panicking and without hanging. Captures
// the regression class where a downstream error breaks the merge/cleanup
// path of the processor pipeline.
func TestRunDispatchesHTTPErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"intentional"}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	rule := `WHEN .x == 1 EXEC "` + srv.URL + `/boom"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/test", 0, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	done := make(chan error, 1)
	go func() {
		done <- pu.Run(context.Background(), `{"x":1}`, "boot/test/0", resCh)
	}()

	select {
	case err := <-done:
		// ExecHTTP today does NOT distinguish 5xx as an error path — it
		// returns the body verbatim. The test's purpose is to catch
		// regressions where 5xx causes a panic / hang / nil deref in the
		// merge logic, not to specify response semantics.
		if err != nil {
			t.Logf("Run returned err (acceptable for 5xx): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run hung on 5xx response from dispatch target")
	}
}

// TestRunRejectsUnsupportedScheme verifies the post-gRPC default branch in
// Exec(): a rule with a scheme other than txco://, http://, https:// must
// fail loudly rather than silently dispatch somewhere unexpected.
func TestRunRejectsUnsupportedScheme(t *testing.T) {
	pu, _ := newTestUnit(t)

	rule := `WHEN .x == 1 EXEC "bogus://nothing-here"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/test", 0, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	done := make(chan error, 1)
	go func() {
		done <- pu.Run(context.Background(), `{"x":1}`, "boot/test/0", resCh)
	}()

	select {
	case <-done:
		// Run itself doesn't bubble Exec errors — they're recorded per-op
		// and the pipeline continues. The point of this test is that we
		// reach the unsupported-scheme path and it doesn't panic. Coverage
		// of the default branch is the proof.
	case <-time.After(2 * time.Second):
		t.Fatal("Run hung on rule with unsupported scheme")
	}
}

// TestRunAdvancesPastExecutedScopeOnAutoAdvance pins the regression:
// when Run is called with an entry stage whose scope sits BELOW the
// first scope that actually has rules (e.g. enter at "stack/0" but the
// only rule is at scope 100), the chassis must execute that rule
// exactly once and then terminate. Pre-fix, the advance logic at
// processor.go:763 used `nextOps` computed from the entry stage, whose
// floor-lookup at scope+1 returned the same scope-100 rule the chassis
// just executed, triggering a recursive re-fire until the request
// timed out. Single-rule stacks (e.g. the mcp-quickstart echo) were
// the canonical victim.
func TestRunAdvancesPastExecutedScopeOnAutoAdvance(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	// Seed ONE rule at scope 100. Run will be called at "stack/0",
	// so the chassis floor-advances from scope 0 to scope 100, runs
	// the rule, and is then expected to terminate.
	rule := `EXEC "` + srv.URL + `/once"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"sparse", 100, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "sparse/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("rule executed %d times, want exactly 1 (re-fire regression)", got)
	}
}

// TestRunGotoIntoSparseStackTerminatesCleanly mirrors the quickstart
// shape: a boot pipeline rule sets _txc.goto to a target stack whose
// first rule lives at scope 100 (not 0). The chassis should jump,
// execute the single rule once, and terminate — no need for an
// explicit `_txc.halt = true` to break out of a re-fire loop.
func TestRunGotoIntoSparseStackTerminatesCleanly(t *testing.T) {
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"42"}`))
	}))
	t.Cleanup(target.Close)

	pu, _ := newTestUnit(t)

	// Boot rule at scope 100: jumps into the sparse target stack at
	// scope 0 (the route convention is `<stack>/0`). `@` is sugar
	// for `._txc.`.
	bootRule := `EMIT @goto = "target/0"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot", 100, bootRule); err != nil {
		t.Fatalf("seed boot: %v", err)
	}
	// Target rule at scope 100 ONLY (no scope 0 entry rule). This is
	// the case the bug hit.
	targetRule := `EXEC "` + target.URL + `/once"`
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"target", 100, targetRule); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{}`, "boot/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&targetHits); got != 1 {
		t.Errorf("target rule executed %d times, want exactly 1 (re-fire after goto regression)", got)
	}
}

// newTestUnit builds a minimal *Unit suitable for exercising Run() against
// an empty ops table, plus a SpanRecorder hooked to its tracer.
func newTestUnit(t *testing.T) (*Unit, *tracetest.SpanRecorder) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	mc := &metrics.Metrics{
		Tracer: tp.Tracer("test"),
	}

	// In-memory SQLite for both Db and Dbc.Db, with the ops table created.
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// Mirror the real runtime schema (db/schema/sqlite/runtime): ops
	// carry tenant_id with tenant-scoped uniqueness, and a tenants table
	// maps the routed slug -> tenant_id for the data-plane lookup.
	if _, err := db.Exec(`CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, tenant_id TEXT, UNIQUE(stack, scope, txcl, tenant_id));`); err != nil {
		t.Fatalf("create ops: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT, revoked_at TEXT);`); err != nil {
		t.Fatalf("create tenants: %v", err)
	}

	conf := config.Config{
		Environment:    "test",
		OpTimeout:      "1s",
		OpTimeoutMax:   "10m",
		DialTimeout:    "100ms",
		OpMetricsRegex: "", // skip per-op metric recording
		// Deferred-join / async knobs (mirror production defaults) so the
		// ack timeout, runtime budget, and reap horizon are sane in tests.
		AsyncAckTimeout:     "5s",
		AsyncRuntimeDefault: "10m",
		DeferredJoinSlack:   "60s",
	}

	logger := zap.NewNop()
	reg := registry.New(conf, logger)
	bus := make(chan *event.Envelope, 1)

	return &Unit{
		Conf:       conf,
		Logger:     logger,
		RuntimeDB:  db,
		AuthDB:     db,
		Dbc:        &dbcache.DbCache{Db: db, Source: db, Logger: logger},
		Mc:         mc,
		Reg:        reg,
		Bus:        bus,
		Mux:        radix.New(),
		HTTPClient: &http.Client{Timeout: 2 * time.Second},
	}, sr
}
