package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestRunBreaksAfterScopeWhenFlagSet exercises the happy path: an
// envelope arriving with both _txc.flag_breakpoint=true and
// _txc.break=100 causes the chassis to run scope 100, merge its result,
// and stop before advancing to scope 200. The emitted response carries
// _txc.broke_at=100 and the internal gating fields are stripped.
func TestRunBreaksAfterScopeWhenFlagSet(t *testing.T) {
	var hits100, hits200 int32

	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits100, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stage100":"ran"}`))
	}))
	t.Cleanup(srv100.Close)
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits200, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stage200":"ran"}`))
	}))
	t.Cleanup(srv200.Close)

	pu, _ := newTestUnit(t)
	seed := func(stack string, scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			stack, scope, name, rule); err != nil {
			t.Fatalf("seed %s/%d/%s: %v", stack, scope, name, err)
		}
	}
	seed("svc", 100, "handler", `EXEC "`+srv100.URL+`"`)
	seed("svc", 200, "after", `EXEC "`+srv200.URL+`"`)

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":100}}`
	if err := pu.Run(context.Background(), in, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits100); got != 1 {
		t.Errorf("stage 100 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits200); got != 0 {
		t.Errorf("stage 200 hits = %d, want 0 (break should have halted before)", got)
	}

	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "_txc.broke_at").Int(); got != 100 {
			t.Errorf("_txc.broke_at = %d, want 100 (body=%s)", got, payload.Raw)
		}
		if gjson.Get(payload.Raw, "_txc.break").Exists() {
			t.Errorf("_txc.break should be stripped after consumption; body=%s", payload.Raw)
		}
		if gjson.Get(payload.Raw, "_txc.flag_breakpoint").Exists() {
			t.Errorf("_txc.flag_breakpoint should be stripped; body=%s", payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "stage100").String(); got != "ran" {
			t.Errorf("expected stage100 field merged in; body=%s", payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunIgnoresBreakWithoutFlag locks in the gate: even when the
// envelope carries _txc.break=100, the processor must NOT halt unless
// _txc.flag_breakpoint is also set. This is the prod-safety property —
// a rule SETting _txc.break in a production chassis has no effect.
func TestRunIgnoresBreakWithoutFlag(t *testing.T) {
	var hits100, hits200 int32
	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits100, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv100.Close)
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits200, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv200.Close)

	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 100, "handler", `EXEC "`+srv100.URL+`"`); err != nil {
		t.Fatalf("seed 100: %v", err)
	}
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 200, "after", `EXEC "`+srv200.URL+`"`); err != nil {
		t.Fatalf("seed 200: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"break":100}}` // _txc.break set, _txc.flag_breakpoint deliberately absent
	if err := pu.Run(context.Background(), in, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits100); got != 1 {
		t.Errorf("stage 100 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits200); got != 1 {
		t.Errorf("stage 200 hits = %d, want 1 (no flag → no break, pipeline must finish)", got)
	}

	select {
	case payload := <-resCh:
		if gjson.Get(payload.Raw, "_txc.broke_at").Exists() {
			t.Errorf("_txc.broke_at should NOT be set when flag is absent; body=%s", payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunBreakHaltsWhenNextWouldOvershoot covers the sparse-scope case
// the user hit: pipeline scopes are 100 and 200, and break=150 (a
// scope that doesn't exist as its own stage). With exact-match
// semantics this would do nothing and the pipeline would run through;
// with the threshold semantic we halt after scope 100's merge because
// the next stage (200) would otherwise cross break.
func TestRunBreakHaltsWhenNextWouldOvershoot(t *testing.T) {
	var hits100, hits200 int32
	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits100, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"at100":true}`))
	}))
	t.Cleanup(srv100.Close)
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits200, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"at200":true}`))
	}))
	t.Cleanup(srv200.Close)

	pu, _ := newTestUnit(t)
	for _, r := range []struct {
		scope int
		name  string
		rule  string
	}{
		{100, "h", `EXEC "` + srv100.URL + `"`},
		{200, "h", `EXEC "` + srv200.URL + `"`},
	} {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"svc", r.scope, r.name, r.rule); err != nil {
			t.Fatalf("seed %d: %v", r.scope, err)
		}
	}

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":150}}`
	if err := pu.Run(context.Background(), in, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits100); got != 1 {
		t.Errorf("svc/100 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits200); got != 0 {
		t.Errorf("svc/200 hits = %d, want 0 (break=150 should halt before crossing)", got)
	}
	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "_txc.broke_at").Int(); got != 100 {
			t.Errorf("_txc.broke_at = %d, want 100 (body=%s)", got, payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunBreakThresholdSurvivesAutoAdvance is the regression guard for
// a real-world bug: the chassis enters each recursive Run with a stage
// that's typically off-by-one from where the rules actually live
// (e.g. stage="svc/101" auto-advances to rules at scope 200). When that
// happens, the outer `nextOps` computed at Run-entry ends up pointing
// at the SAME scope we're about to run — making nextScope == curScope.
// Without re-querying inside the break check, sparse pipelines like
// {100, 200, 1000} never see the "next > N" path fire and break gets
// applied at a later scope than the developer intended.
//
// Three rules at scopes 100/200/1000; break=150; enter at scope 0 so
// the chassis must auto-advance. Expected: halt after scope 100 with
// broke_at=100 (the latest scope ≤ 150). Before the re-query fix, the
// pipeline would advance past 100 and halt at 200 via the cur>=N path.
func TestRunBreakThresholdSurvivesAutoAdvance(t *testing.T) {
	var hits100, hits200, hits1000 int32
	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits100, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"at100":true}`))
	}))
	t.Cleanup(srv100.Close)
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits200, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"at200":true}`))
	}))
	t.Cleanup(srv200.Close)
	srv1000 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits1000, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"at1000":true}`))
	}))
	t.Cleanup(srv1000.Close)

	pu, _ := newTestUnit(t)
	for _, r := range []struct {
		scope int
		url   string
	}{
		{100, srv100.URL},
		{200, srv200.URL},
		{1000, srv1000.URL},
	} {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"svc", r.scope, "h", `EXEC "`+r.url+`"`); err != nil {
			t.Fatalf("seed %d: %v", r.scope, err)
		}
	}

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":150}}`
	// Enter at scope 0 — forces the chassis to auto-advance through the
	// stage sequence, exercising the path where Run-entry `nextOps` is
	// stale.
	if err := pu.Run(context.Background(), in, "svc/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits100); got != 1 {
		t.Errorf("svc/100 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits200); got != 0 {
		t.Errorf("svc/200 hits = %d, want 0 (break=150 should halt at 100, never run 200)", got)
	}
	if got := atomic.LoadInt32(&hits1000); got != 0 {
		t.Errorf("svc/1000 hits = %d, want 0", got)
	}
	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "_txc.broke_at").Int(); got != 100 {
			t.Errorf("_txc.broke_at = %d, want 100 (body=%s)", got, payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunBreakDoesNotHaltPastLastStage verifies the inverse: when break
// is set past every scope the pipeline contains (e.g. break=999 in a
// stack whose only rule is at scope 100), the pipeline runs to natural
// completion. No broke_at is stamped — the user wanted to halt past
// the end, which is the same as not breaking.
func TestRunBreakDoesNotHaltPastLastStage(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"final":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"svc", 100, "only", `EXEC "`+srv.URL+`"`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":999}}`
	if err := pu.Run(context.Background(), in, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("handler hits = %d, want 1", got)
	}
	select {
	case payload := <-resCh:
		if gjson.Get(payload.Raw, "_txc.broke_at").Exists() {
			t.Errorf("_txc.broke_at should NOT be set when break is past end; body=%s", payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "final").Bool(); !got {
			t.Errorf("expected final field from natural pipeline end; body=%s", payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunBreakIncludesOpstack verifies break responses carry the
// pipeline-shape summary (per-step op names) for the current stack.
// That's what developers consult to figure out which scopes are even
// available to break at.
func TestRunBreakIncludesOpstack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	// Two parallel ops at scope 100, one at 200, one at 1000. Each rule
	// gets a distinct URL path so the (stack, scope, txcl) UNIQUE
	// constraint is satisfied for the parallel pair at scope 100.
	for _, r := range []struct {
		scope int
		name  string
	}{
		{100, "hello"},
		{100, "world"},
		{200, "sort"},
		{1000, "render"},
	} {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			"hello-world", r.scope, r.name, `EXEC "`+srv.URL+`/`+r.name+`"`); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":100}}`
	if err := pu.Run(context.Background(), in, "hello-world/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		opstack := gjson.Get(payload.Raw, "_txc.opstack")
		if !opstack.IsArray() {
			t.Fatalf("_txc.opstack missing or not array; body=%s", payload.Raw)
		}
		entries := opstack.Array()
		if len(entries) != 3 {
			t.Errorf("opstack has %d entries, want 3 (steps 100/200/1000); got %s", len(entries), opstack.Raw)
		}
		// Step 100 should list both hello + world.
		step0 := entries[0]
		if got := step0.Get("step").Int(); got != 100 {
			t.Errorf("opstack[0].step = %d, want 100", got)
		}
		ops0 := step0.Get("ops").Array()
		if len(ops0) != 2 {
			t.Errorf("opstack[0].ops len = %d, want 2 (hello + world); got %s", len(ops0), step0.Raw)
		}
		names := map[string]bool{}
		for _, n := range ops0 {
			names[n.String()] = true
		}
		if !names["hello"] || !names["world"] {
			t.Errorf("opstack[0].ops = %s, want hello + world", step0.Get("ops").Raw)
		}
		// Step 200 → sort. Step 1000 → render.
		if got := entries[1].Get("step").Int(); got != 200 {
			t.Errorf("opstack[1].step = %d, want 200", got)
		}
		if got := entries[1].Get("ops.0").String(); got != "sort" {
			t.Errorf("opstack[1].ops[0] = %q, want sort", got)
		}
		if got := entries[2].Get("step").Int(); got != 1000 {
			t.Errorf("opstack[2].step = %d, want 1000", got)
		}
		if got := entries[2].Get("ops.0").String(); got != "render" {
			t.Errorf("opstack[2].ops[0] = %q, want render", got)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}

// TestRunBreakSurvivesStageJump verifies break works across the
// canonical boot→service routing pattern: a boot rule stage-jumps to
// the service stack via EXEC "svc/100"; break=100 must halt at scope
// 100 (post-merge) instead of advancing to 200.
func TestRunBreakSurvivesStageJump(t *testing.T) {
	var hits100, hits200 int32
	srv100 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits100, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"first":true}`))
	}))
	t.Cleanup(srv100.Close)
	srv200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits200, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"second":true}`))
	}))
	t.Cleanup(srv200.Close)

	pu, _ := newTestUnit(t)
	seed := func(stack string, scope int, name, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
			stack, scope, name, rule); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Boot stage-jumps to svc/100 (canonical pattern).
	seed("boot", 0, "router", `EXEC "svc/100"`)
	seed("svc", 100, "first", `EXEC "`+srv100.URL+`"`)
	seed("svc", 200, "second", `EXEC "`+srv200.URL+`"`)

	resCh := make(chan event.Payload, 1)
	in := `{"_txc":{"flag_breakpoint":true,"break":100}}`
	if err := pu.Run(context.Background(), in, "boot/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits100); got != 1 {
		t.Errorf("svc/100 hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hits200); got != 0 {
		t.Errorf("svc/200 hits = %d, want 0 (break should halt before)", got)
	}
	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "_txc.broke_at").Int(); got != 100 {
			t.Errorf("_txc.broke_at = %d, want 100 (body=%s)", got, payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "first").Bool(); !got {
			t.Errorf("expected first=true merged from svc/100; body=%s", payload.Raw)
		}
	default:
		t.Error("expected a payload on resCh")
	}
}
