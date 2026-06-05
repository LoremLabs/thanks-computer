package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// writeTraceFixture lays down a minimal trace tree under traceDir/requests/<rid>
// matching the format chassis/trace/file.go produces. Two steps; ?include=full
// will pick up payload files when stepWithPayloads is true.
func writeTraceFixture(t *testing.T, traceDir, rid string, stepWithPayloads bool) {
	t.Helper()
	reqDir := filepath.Join(traceDir, "requests", rid)
	steps := filepath.Join(reqDir, "steps")
	if err := os.MkdirAll(steps, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Request-level in.json.
	in := `{
  "rid": "` + rid + `",
  "src": "web POST /api/foo",
  "tenant": "acme",
  "stack": "default",
  "started_at": "2026-05-12T15:33:08.142Z",
  "payload_bytes": 1234,
  "payload": {"hello": "world"}
}`
	mustWrite(t, filepath.Join(reqDir, "in.json"), in)

	// Final out.json — only consulted on ?include=full.
	mustWrite(t, filepath.Join(reqDir, "out.json"), `{"ok": true}`)

	// timeline.jsonl: request.start, a stage.jump (so Route gets
	// populated), and request.end. The handler should ignore other
	// events.
	timeline := `{"ts":"2026-05-12T15:33:08.142Z","event":"request.start","rid":"` + rid + `"}
{"ts":"2026-05-12T15:33:08.150Z","event":"stage.jump","from":"boot/web/0","to":"hello-world/0"}
{"ts":"2026-05-12T15:33:08.184Z","event":"request.end","status":"ok","duration_ms":42}
`
	mustWrite(t, filepath.Join(reqDir, "timeline.jsonl"), timeline)

	// Two step folders. Folder names are <scope-padded>-<name>; lexical
	// sort puts 0100-parse-input before 0200-call-backend.
	s1 := filepath.Join(steps, "0100-parse-input")
	s2 := filepath.Join(steps, "0200-call-backend")
	if err := os.MkdirAll(s1, 0o755); err != nil {
		t.Fatalf("mkdir step1: %v", err)
	}
	if err := os.MkdirAll(s2, 0o755); err != nil {
		t.Fatalf("mkdir step2: %v", err)
	}
	mustWrite(t, filepath.Join(s1, "meta.json"), `{
  "trace_id": "`+rid+`",
  "stack": "default",
  "scope": 100,
  "name": "parse-input",
  "operation": "txcl://parse",
  "transport": "txcl",
  "started_at": "2026-05-12T15:33:08.144Z",
  "finished_at": "2026-05-12T15:33:08.156Z",
  "duration_ms": 12,
  "status": "ok",
  "input_bytes": 1234,
  "output_bytes": 1456
}`)
	mustWrite(t, filepath.Join(s2, "meta.json"), `{
  "trace_id": "`+rid+`",
  "stack": "default",
  "scope": 200,
  "name": "call-backend",
  "operation": "https://api.x/foo",
  "transport": "http",
  "started_at": "2026-05-12T15:33:08.160Z",
  "finished_at": "2026-05-12T15:33:08.178Z",
  "duration_ms": 18,
  "status": "ok",
  "input_bytes": 1456,
  "output_bytes": 2100
}`)
	if stepWithPayloads {
		mustWrite(t, filepath.Join(s1, "in.json"), `{"step1in": 1}`)
		mustWrite(t, filepath.Join(s1, "out.json"), `{"step1out": 1}`)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestHandleTraceRequest_Summary(t *testing.T) {
	dir := t.TempDir()
	rid := "CawWjXCkFe7p3F3E5rCgM"
	writeTraceFixture(t, dir, rid, false)

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "summary",
	})

	req := httptest.NewRequest(http.MethodGet, "/traces/requests/"+rid+".json", nil)
	req = mux.SetURLVars(req, map[string]string{"rid": rid})
	w := httptest.NewRecorder()
	c.handleTraceRequest(w, asSuperAdmin(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}

	var resp traceRequestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RID != rid {
		t.Errorf("rid = %q, want %q", resp.RID, rid)
	}
	if resp.Src != "web POST /api/foo" {
		t.Errorf("src = %q", resp.Src)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if resp.DurationMs == nil || *resp.DurationMs != 42 {
		t.Errorf("duration_ms = %v, want 42", resp.DurationMs)
	}
	if resp.Route != "hello-world/0" {
		t.Errorf("route = %q, want hello-world/0 (first stage.jump destination)", resp.Route)
	}
	if resp.TraceMode != "summary" {
		t.Errorf("trace_mode = %q, want summary", resp.TraceMode)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("steps len = %d, want 2", len(resp.Steps))
	}
	if resp.Steps[0].Scope != 100 || resp.Steps[1].Scope != 200 {
		t.Errorf("scope ordering: got %d, %d", resp.Steps[0].Scope, resp.Steps[1].Scope)
	}
	if resp.Steps[0].Name != "parse-input" {
		t.Errorf("step1 name = %q", resp.Steps[0].Name)
	}
	if resp.Steps[1].Operation != "https://api.x/foo" {
		t.Errorf("step2 op = %q", resp.Steps[1].Operation)
	}
	// Summary mode: no payload fields.
	if resp.In != nil || resp.Out != nil {
		t.Errorf("summary mode should not embed in/out, got in=%v out=%v", resp.In, resp.Out)
	}
}

func TestHandleTraceRequest_IncludeFull(t *testing.T) {
	dir := t.TempDir()
	rid := "CawWjXCkFe7p3F3E5rCgM"
	writeTraceFixture(t, dir, rid, true)

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "full",
	})

	req := httptest.NewRequest(http.MethodGet, "/traces/requests/"+rid+".json?include=full", nil)
	req = mux.SetURLVars(req, map[string]string{"rid": rid})
	w := httptest.NewRecorder()
	c.handleTraceRequest(w, asSuperAdmin(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceRequestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.In == nil {
		t.Error("include=full: expected top-level `in` payload")
	}
	if resp.Out == nil {
		t.Error("include=full: expected top-level `out` payload")
	}
	if resp.Steps[0].In == nil {
		t.Error("include=full: expected step1.in payload")
	}
	if resp.Steps[0].Out == nil {
		t.Error("include=full: expected step1.out payload")
	}
}

func TestHandleTraceRequest_NotFound(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "summary",
	})
	req := httptest.NewRequest(http.MethodGet, "/traces/requests/MISSING.json", nil)
	req = mux.SetURLVars(req, map[string]string{"rid": "MISSING"})
	w := httptest.NewRecorder()
	c.handleTraceRequest(w, asSuperAdmin(req))
	if w.Code != http.StatusNotFound {
		t.Fatalf("got status %d, want 404", w.Code)
	}
}

func TestHandleTraceRequest_BadRID(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      t.TempDir(),
		TraceMode:     "summary",
	})
	req := httptest.NewRequest(http.MethodGet, "/traces/requests/..%2Fetc.json", nil)
	req = mux.SetURLVars(req, map[string]string{"rid": "../etc"})
	w := httptest.NewRecorder()
	c.handleTraceRequest(w, asSuperAdmin(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("got status %d, want 400", w.Code)
	}
}

func TestHandleTraceList(t *testing.T) {
	dir := t.TempDir()
	// Two finished hxid traces + one resume trace (non-time-sortable
	// RID) + one in-flight. Ordering must be by mtime, not name — a
	// name sort clumps the `resume-` rid apart from the hxid ones
	// instead of interleaving by time.
	for _, rid := range []string{"CawWfinished2", "CawWfinished1"} {
		writeTraceFixture(t, dir, rid, false)
	}
	resumeRID := "resume-run_Cb4xZqWmtraceX-hello-world_300"
	writeTraceFixture(t, dir, resumeRID, false)
	// In-flight: in.json + start-only timeline, no request.end.
	inflight := "CawWinflight0"
	reqDir := filepath.Join(dir, "requests", inflight)
	if err := os.MkdirAll(filepath.Join(reqDir, "steps"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(reqDir, "in.json"),
		`{"rid":"`+inflight+`","src":"web","tenant":"t","stack":"s","started_at":"2026-05-12T15:33:08.142Z","payload_bytes":0,"payload":null}`)
	mustWrite(t, filepath.Join(reqDir, "timeline.jsonl"),
		`{"ts":"2026-05-12T15:33:08.142Z","event":"request.start","rid":"`+inflight+`"}`+"\n")

	// Explicit mtimes encode chronology (oldest → newest). The resume
	// trace lands BETWEEN the two hxid finished traces in time, so a
	// correct (mtime) sort interleaves it; a name sort would not.
	base := time.Now().Add(-time.Hour)
	order := []string{"CawWfinished1", "CawWfinished2", resumeRID, inflight}
	for i, rid := range order {
		when := base.Add(time.Duration(i) * 10 * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, "requests", rid), when, when); err != nil {
			t.Fatalf("chtimes %s: %v", rid, err)
		}
	}

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "full",
	})

	req := httptest.NewRequest(http.MethodGet, "/traces/requests.json", nil)
	w := httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 4 {
		t.Errorf("total = %d, want 4", resp.Total)
	}
	if len(resp.Traces) != 4 {
		t.Fatalf("traces len = %d, want 4", len(resp.Traces))
	}
	// Strict newest-first by mtime — resume trace interleaved by time,
	// NOT clustered by its non-hxid name.
	wantOrder := []string{inflight, resumeRID, "CawWfinished2", "CawWfinished1"}
	for i, want := range wantOrder {
		if resp.Traces[i].RID != want {
			t.Fatalf("traces[%d].rid = %q, want %q (newest-first by time)", i, resp.Traces[i].RID, want)
		}
	}
	// A finished entry carries status + duration + route.
	f2 := resp.Traces[2] // CawWfinished2
	if f2.Status != "ok" {
		t.Errorf("finished status = %q, want ok", f2.Status)
	}
	if f2.DurationMs == nil || *f2.DurationMs != 42 {
		t.Errorf("finished duration_ms = %v, want 42", f2.DurationMs)
	}
	if f2.Route != "hello-world/0" {
		t.Errorf("finished route = %q, want hello-world/0", f2.Route)
	}
	// In-flight entry: status fallback, no duration.
	if resp.Traces[0].Status != "in-flight" {
		t.Errorf("inflight status = %q, want in-flight", resp.Traces[0].Status)
	}
	if resp.Traces[0].DurationMs != nil {
		t.Errorf("inflight duration should be nil, got %v", *resp.Traces[0].DurationMs)
	}
}

func TestHandleTraceList_Grep(t *testing.T) {
	dir := t.TempDir()
	// writeTraceFixture writes two steps: parse-input (txcl://parse)
	// and call-backend (https://api.x/foo). Same for every fixture —
	// so we'd get all 3 as matches for both "parse" and "api". Use
	// targeted writes to give us heterogeneous data.
	mustWriteFixtureWithSteps := func(rid string, stepName, stepOp, payload string) {
		reqDir := filepath.Join(dir, "requests", rid)
		if err := os.MkdirAll(filepath.Join(reqDir, "steps", "0100-"+stepName), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		mustWrite(t, filepath.Join(reqDir, "in.json"),
			`{"rid":"`+rid+`","src":"http","tenant":"","stack":"boot/%/0","started_at":"2026-05-12T15:33:08.142Z","payload_bytes":0,"payload":`+payload+`}`)
		mustWrite(t, filepath.Join(reqDir, "timeline.jsonl"),
			`{"ts":"2026-05-12T15:33:08.142Z","event":"request.start","rid":"`+rid+`"}
{"ts":"2026-05-12T15:33:08.184Z","event":"request.end","status":"ok","duration_ms":42}
`)
		mustWrite(t, filepath.Join(reqDir, "steps", "0100-"+stepName, "meta.json"),
			`{"trace_id":"`+rid+`","stack":"flows","scope":100,"name":"`+stepName+`","operation":"`+stepOp+`","transport":"http","started_at":"2026-05-12T15:33:08.144Z","finished_at":"2026-05-12T15:33:08.156Z","duration_ms":12,"status":"ok","input_bytes":1,"output_bytes":1}`)
	}

	mustWriteFixtureWithSteps("CaxStripe003", "stripe-charge", "https://api.stripe.com/v1/charges", `null`)
	mustWriteFixtureWithSteps("CaxHello002", "hello", "https://api.example.com/words/hello", `null`)
	mustWriteFixtureWithSteps("CaxWorld001", "world", "https://api.example.com/words/world", `{"url":{"path":"/zztop"}}`)

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "summary",
	})

	// grep=stripe → only the Stripe trace matches.
	req := httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=stripe", nil)
	w := httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))
	var resp traceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Traces) != 1 || resp.Traces[0].RID != "CaxStripe003" {
		t.Errorf("grep=stripe: got total=%d traces=%d first=%q",
			resp.Total, len(resp.Traces), firstRID(resp.Traces))
	}

	// grep=api → all three (each has an "api" substring in operation).
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=api", nil)
	c.handleTraceList(w, asSuperAdmin(req))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 3 || len(resp.Traces) != 3 {
		t.Errorf("grep=api: got total=%d traces=%d, want 3 each", resp.Total, len(resp.Traces))
	}

	// grep=NoSuchThing → empty result, total=0.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=NoSuchThing", nil)
	c.handleTraceList(w, asSuperAdmin(req))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || len(resp.Traces) != 0 {
		t.Errorf("grep=NoSuchThing: got total=%d traces=%d, want 0", resp.Total, len(resp.Traces))
	}

	// Match on step name (not just operation): "world" matches the
	// "world" name + "words/world" operation.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=world", nil)
	c.handleTraceList(w, asSuperAdmin(req))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Traces[0].RID != "CaxWorld001" {
		t.Errorf("grep=world: got total=%d first=%q, want 1 CaxWorld001",
			resp.Total, firstRID(resp.Traces))
	}

	// Match inside the request payload (in.json), not just step
	// metadata. The "zztop" string only appears in CaxWorld001's
	// in.json — exercises the broader file-walk grep.
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=zztop", nil)
	c.handleTraceList(w, asSuperAdmin(req))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || resp.Traces[0].RID != "CaxWorld001" {
		t.Errorf("grep=zztop (payload match): got total=%d first=%q, want 1 CaxWorld001",
			resp.Total, firstRID(resp.Traces))
	}

	// Case-insensitive: "ZZTOP" matches the same as "zztop".
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=ZZTOP", nil)
	c.handleTraceList(w, asSuperAdmin(req))
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Errorf("grep=ZZTOP (case-insensitive): got total=%d, want 1", resp.Total)
	}
}

func firstRID(traces []traceListEntry) string {
	if len(traces) == 0 {
		return ""
	}
	return traces[0].RID
}

// TestHandleTraceList_ETag confirms the cheap precheck:
//   - response carries an ETag header
//   - resending it via If-None-Match returns 304 with no body
//   - touching a relevant file (timeline.jsonl mtime change) flips
//     the ETag so the next request gets a fresh 200
//   - changing the grep query param invalidates the ETag even when
//     on-disk state didn't change
func TestHandleTraceList_ETag(t *testing.T) {
	dir := t.TempDir()
	writeTraceFixture(t, dir, "CawWfixture001", false)

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "summary",
	})

	// First request → 200 + ETag.
	req := httptest.NewRequest(http.MethodGet, "/traces/requests.json", nil)
	w := httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))
	if w.Code != http.StatusOK {
		t.Fatalf("first request: status %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected ETag header on first response")
	}

	// Second request with If-None-Match: <etag> → 304, empty body.
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))
	if w.Code != http.StatusNotModified {
		t.Errorf("matching ETag: status %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 response should have empty body, got %d bytes", w.Body.Len())
	}

	// Touch the timeline.jsonl file → ETag should change.
	timelinePath := filepath.Join(dir, "requests", "CawWfixture001", "timeline.jsonl")
	future := mustTouch(t, timelinePath)
	_ = future

	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))
	if w.Code != http.StatusOK {
		t.Errorf("after touch: status %d, want 200 (etag should have changed)", w.Code)
	}
	newEtag := w.Header().Get("ETag")
	if newEtag == etag {
		t.Errorf("ETag did not change after touching timeline.jsonl")
	}

	// Changing grep alone (no on-disk change) → ETag differs.
	req = httptest.NewRequest(http.MethodGet, "/traces/requests.json?grep=foo", nil)
	w = httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))
	if w.Code != http.StatusOK {
		t.Fatalf("grep request: status %d", w.Code)
	}
	grepEtag := w.Header().Get("ETag")
	if grepEtag == newEtag {
		t.Errorf("grep query should yield a different ETag from unfiltered")
	}
}

// mustTouch bumps a file's mtime by one second. Returns the new mtime
// for the caller to assert against if needed.
func mustTouch(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	next := info.ModTime().Add(time.Second)
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return next
}

func TestHandleTraceList_Limit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		writeTraceFixture(t, dir, "Trace"+strconv.Itoa(i), false)
	}
	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "summary",
	})
	req := httptest.NewRequest(http.MethodGet, "/traces/requests.json?limit=2", nil)
	w := httptest.NewRecorder()
	c.handleTraceList(w, asSuperAdmin(req))

	var resp traceListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 5 {
		t.Errorf("total = %d, want 5", resp.Total)
	}
	if len(resp.Traces) != 2 {
		t.Errorf("len(traces) = %d, want 2 (limited)", len(resp.Traces))
	}
}

func TestHandleTraceRequest_InFlight(t *testing.T) {
	// A trace with no request.end line in the timeline should surface
	// status="in-flight" with no duration.
	dir := t.TempDir()
	rid := "CawWjXCkFe7p3F3E5rCgM"
	reqDir := filepath.Join(dir, "requests", rid, "steps")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWrite(t, filepath.Join(dir, "requests", rid, "in.json"),
		`{"rid":"`+rid+`","src":"web","tenant":"t","stack":"s","started_at":"2026-05-12T15:33:08.142Z","payload_bytes":0,"payload":null}`)
	mustWrite(t, filepath.Join(dir, "requests", rid, "timeline.jsonl"),
		`{"ts":"2026-05-12T15:33:08.142Z","event":"request.start","rid":"`+rid+`"}`+"\n")

	c := newTestController(t, config.Config{
		Personalities: "admin",
		TraceDir:      dir,
		TraceMode:     "full",
	})
	req := httptest.NewRequest(http.MethodGet, "/traces/requests/"+rid+".json", nil)
	req = mux.SetURLVars(req, map[string]string{"rid": rid})
	w := httptest.NewRecorder()
	c.handleTraceRequest(w, asSuperAdmin(req))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceRequestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "in-flight" {
		t.Errorf("status = %q, want in-flight", resp.Status)
	}
	if resp.DurationMs != nil {
		t.Errorf("duration_ms should be nil for in-flight, got %v", *resp.DurationMs)
	}
}

// TestHandleTraceRequest_TenantIsolation is the leak fix at the handler: a
// trace resolved to prod-mankins is readable by that tenant-owner but returns
// 404 (no existence leak) to a different tenant.
func TestHandleTraceRequest_TenantIsolation(t *testing.T) {
	dir := t.TempDir()
	sink, err := trace.NewFileSink(dir, trace.ModeSummary)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	// Enters pinned to _sys, resolves to prod-mankins via request.usage.
	tr := sink.Begin(trace.RequestInfo{
		RID: "rid-mankins", Src: "http", Tenant: "_sys", Stack: "boot/0",
		StartedAt: time.Now(), Payload: []byte(`{}`),
	})
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "request.usage", Fields: map[string]any{"tenant": "prod-mankins"}})
	tr.End("ok", "", []byte(`{}`))

	c := newTestController(t, config.Config{Personalities: "admin", TraceDir: dir, TraceMode: "summary"})

	get := func(tenantSlug string) int {
		req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/x", nil), map[string]string{"rid": "rid-mankins"})
		w := httptest.NewRecorder()
		c.handleTraceRequest(w, asTenant(req, tenantSlug))
		return w.Code
	}

	if code := get("prod-mankins"); code != http.StatusOK {
		t.Errorf("owner read: status %d, want 200", code)
	}
	if code := get("acme"); code != http.StatusNotFound {
		t.Errorf("cross-tenant read: status %d, want 404", code)
	}
}
