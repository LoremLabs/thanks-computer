package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newSink(t *testing.T, mode Mode) (*FileSink, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFileSink(dir, mode)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	return s, dir
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, b)
	}
	return m
}

// TestFileSinkFullModeLayout drives the sink through a small "request"
// (Begin → two Steps → End) in ModeFull and asserts every artifact the
// plan promises lands on disk with the expected shape.
func TestFileSinkFullModeLayout(t *testing.T) {
	s, dir := newSink(t, ModeFull)
	start := time.Now()
	tr := s.Begin(RequestInfo{
		RID:       "req-001",
		Src:       "http",
		Tenant:    "demo",
		Stack:     "hello-world",
		StartedAt: start,
		Payload:   []byte(`{"_txc":{"src":"http","rid":"req-001"}}`),
	})

	tr.Step(StepInfo{
		Stack: "hello-world", Scope: 100, Name: "hello",
		Operation:  "http://x/hello",
		Transport:  "http",
		Txcl:       `EXEC "op://HELLO"`,
		Input:      []byte(`{"_txc":{"op":"hello-world/hello","step":100}}`),
		Output:     []byte(`{"words":["hello"]}`),
		StartedAt:  start,
		FinishedAt: start.Add(2 * time.Millisecond),
		Status:     "ok",
	})
	tr.Step(StepInfo{
		Stack: "hello-world", Scope: 200, Name: "sort",
		Operation:  "http://x/sort",
		Transport:  "http",
		Input:      []byte(`{"words":["hello","world"]}`),
		Output:     []byte(`{"sorted_words":["hello","world"]}`),
		StartedAt:  start.Add(3 * time.Millisecond),
		FinishedAt: start.Add(4 * time.Millisecond),
		Status:     "ok",
	})
	tr.End("ok", []byte(`{"sorted_words":["hello","world"]}`))

	reqDir := filepath.Join(dir, "requests", "req-001")

	// Top-level artifacts present.
	for _, name := range []string{"in.json", "out.json", "timeline.jsonl"} {
		p := filepath.Join(reqDir, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}

	// in.json carries the inlet's identity + raw payload.
	req := readJSON(t, filepath.Join(reqDir, "in.json"))
	if req["rid"] != "req-001" {
		t.Errorf("in.json rid=%v, want req-001", req["rid"])
	}
	if req["src"] != "http" {
		t.Errorf("in.json src=%v, want http", req["src"])
	}

	// steps/ has exactly the two we wrote, in order. Folder names are
	// "<scope-padded>-<name>": the scope (zero-padded to four digits)
	// matches the rule's on-disk path so the trace tree reads like the
	// source tree.
	entries, err := os.ReadDir(filepath.Join(reqDir, "steps"))
	if err != nil {
		t.Fatalf("read steps: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("steps count = %d, want 2", len(entries))
	}
	if entries[0].Name() != "0100-hello" {
		t.Errorf("steps[0] = %q, want 0100-hello", entries[0].Name())
	}
	if entries[1].Name() != "0200-sort" {
		t.Errorf("steps[1] = %q, want 0200-sort", entries[1].Name())
	}

	// Each step folder has all four expected files in full mode.
	for _, e := range entries {
		stepDir := filepath.Join(reqDir, "steps", e.Name())
		for _, f := range []string{"op.json", "in.json", "out.json", "meta.json"} {
			if _, err := os.Stat(filepath.Join(stepDir, f)); err != nil {
				t.Errorf("step %s missing %s: %v", e.Name(), f, err)
			}
		}
	}

	// meta.json shape on the first step. No `step` key — scope is the
	// step identity now and lives under "scope".
	meta := readJSON(t, filepath.Join(reqDir, "steps", entries[0].Name(), "meta.json"))
	wantKeys := []string{
		"trace_id", "stack", "scope", "name", "operation", "transport",
		"started_at", "finished_at", "duration_ms",
		"status", "input_bytes", "output_bytes",
	}
	for _, k := range wantKeys {
		if _, ok := meta[k]; !ok {
			t.Errorf("meta.json missing key %q (got keys=%v)", k, mapKeys(meta))
		}
	}
	if _, ok := meta["step"]; ok {
		t.Errorf("meta.json should not have 'step' key — scope is the step")
	}
	if meta["status"] != "ok" {
		t.Errorf("meta status = %v, want ok", meta["status"])
	}
	if meta["transport"] != "http" {
		t.Errorf("meta transport = %v, want http", meta["transport"])
	}

	// timeline.jsonl should have at least request.start, step.start x2,
	// step.end x2, request.end. We don't pin exact count (in case future
	// emits add lines); we pin that each event-type appears at least once.
	tlb, err := os.ReadFile(filepath.Join(reqDir, "timeline.jsonl"))
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	tlStr := string(tlb)
	for _, ev := range []string{"request.start", "step.start", "step.end", "request.end"} {
		if !strings.Contains(tlStr, `"event":"`+ev+`"`) {
			t.Errorf("timeline missing %q event\n%s", ev, tlStr)
		}
	}
}

// TestFileSinkPrettifiesBodies asserts that in.json (at the request
// root and per step), out.json (at the request root and per step),
// and the embedded payload inside the root in.json are all indented
// for human reading, even when the upstream wrote compact JSON.
// timeline.jsonl stays compact (JSONL convention is one event per line).
func TestFileSinkPrettifiesBodies(t *testing.T) {
	s, dir := newSink(t, ModeFull)
	now := time.Now()
	// Compact upstream JSON — this is what real handlers emit.
	compactPayload := []byte(`{"_txc":{"src":"http","rid":"r1"},"data":[1,2,3]}`)
	compactStepIn := []byte(`{"k":1,"nested":{"a":"b","c":[true,false]}}`)
	compactStepOut := []byte(`{"result":"ok","items":[{"id":1},{"id":2}]}`)
	compactFinal := []byte(`{"final":true,"merged":{"x":42}}`)

	tr := s.Begin(RequestInfo{
		RID: "pretty", Src: "http", StartedAt: now, Payload: compactPayload,
	})
	tr.Step(StepInfo{
		Stack: "s", Scope: 100, Name: "n",
		Operation: "http://x", Transport: "http",
		Input:     compactStepIn,
		Output:    compactStepOut,
		StartedAt: now, FinishedAt: now,
		Status: "ok",
	})
	tr.End("ok", compactFinal)

	reqDir := filepath.Join(dir, "requests", "pretty")

	// root in.json should embed payload with proper indentation. Look
	// for a newline + spaces inside the payload structure — compact
	// form would have no newlines anywhere in the payload's portion.
	reqBytes, _ := os.ReadFile(filepath.Join(reqDir, "in.json"))
	if !strings.Contains(string(reqBytes), "\n      \"src\": \"http\"") &&
		!strings.Contains(string(reqBytes), "\n    \"src\": \"http\"") {
		t.Errorf("root in.json payload not pretty-printed (looking for indented _txc.src):\n%s", reqBytes)
	}

	// step in.json: should be multi-line indented.
	stepDirs, _ := os.ReadDir(filepath.Join(reqDir, "steps"))
	stepDir := filepath.Join(reqDir, "steps", stepDirs[0].Name())
	in, _ := os.ReadFile(filepath.Join(stepDir, "in.json"))
	if !strings.Contains(string(in), "\n") {
		t.Errorf("step in.json is still compact (no newlines):\n%s", in)
	}
	if !strings.Contains(string(in), "  \"k\":") {
		t.Errorf("step in.json missing 2-space indent:\n%s", in)
	}

	// step out.json: same.
	out, _ := os.ReadFile(filepath.Join(stepDir, "out.json"))
	if !strings.Contains(string(out), "\n") {
		t.Errorf("step out.json is still compact:\n%s", out)
	}

	// root out.json: same.
	rootOut, _ := os.ReadFile(filepath.Join(reqDir, "out.json"))
	if !strings.Contains(string(rootOut), "\n") {
		t.Errorf("root out.json is still compact:\n%s", rootOut)
	}

	// timeline.jsonl: should STAY compact — one event per line, no
	// internal newlines per event.
	tl, _ := os.ReadFile(filepath.Join(reqDir, "timeline.jsonl"))
	for _, line := range strings.Split(strings.TrimRight(string(tl), "\n"), "\n") {
		if strings.Contains(line, "\n") {
			t.Errorf("timeline.jsonl event spans multiple lines (should be compact JSONL): %s", line)
		}
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Errorf("timeline.jsonl line not a compact JSON object: %s", line)
		}
	}
}

// TestFileSinkPrettifyToleratesNonJSON locks in that non-JSON bytes
// (e.g. a truncated tail from BodyCapBytes) are written as-is rather
// than blown away.
func TestFileSinkPrettifyToleratesNonJSON(t *testing.T) {
	s, dir := newSink(t, ModeFull)
	now := time.Now()
	notJSON := []byte(`{"this":"is truncated mid`)
	tr := s.Begin(RequestInfo{RID: "notjson", StartedAt: now, Payload: []byte(`{}`)})
	tr.Step(StepInfo{
		Stack: "s", Scope: 100, Name: "n",
		Input:     notJSON,
		Output:    notJSON,
		StartedAt: now, FinishedAt: now,
		Status: "ok",
	})
	tr.End("ok", []byte(`{}`))

	stepDirs, _ := os.ReadDir(filepath.Join(dir, "requests", "notjson", "steps"))
	stepDir := filepath.Join(dir, "requests", "notjson", "steps", stepDirs[0].Name())
	in, _ := os.ReadFile(filepath.Join(stepDir, "in.json"))
	if string(in) != string(notJSON) {
		t.Errorf("non-JSON input was modified by prettify; got %q want %q", in, notJSON)
	}
}

// TestFileSinkSummaryModeOmitsBytes locks in that summary writes
// meta.json (so you can see what ran) but NOT the byte payloads.
func TestFileSinkSummaryModeOmitsBytes(t *testing.T) {
	s, dir := newSink(t, ModeSummary)
	now := time.Now()
	tr := s.Begin(RequestInfo{RID: "summary", Src: "http", StartedAt: now, Payload: []byte(`{}`)})
	tr.Step(StepInfo{
		Stack: "s", Scope: 100, Name: "n",
		Operation: "http://x", Transport: "http",
		Input:     []byte("inbytes"),
		Output:    []byte("outbytes"),
		StartedAt: now, FinishedAt: now,
		Status: "ok",
	})
	tr.End("ok", []byte(`{}`))

	stepDirs, _ := os.ReadDir(filepath.Join(dir, "requests", "summary", "steps"))
	if len(stepDirs) != 1 {
		t.Fatalf("steps count = %d, want 1", len(stepDirs))
	}
	stepDir := filepath.Join(dir, "requests", "summary", "steps", stepDirs[0].Name())

	if _, err := os.Stat(filepath.Join(stepDir, "meta.json")); err != nil {
		t.Errorf("meta.json should exist in summary mode: %v", err)
	}
	for _, f := range []string{"op.json", "in.json", "out.json"} {
		if _, err := os.Stat(filepath.Join(stepDir, f)); err == nil {
			t.Errorf("summary mode should NOT write %s, but it exists", f)
		}
	}

	// meta.json still records the sizes so devs see "ah, 7 bytes in" without
	// the bytes themselves.
	meta := readJSON(t, filepath.Join(stepDir, "meta.json"))
	if meta["input_bytes"].(float64) != float64(len("inbytes")) {
		t.Errorf("meta.input_bytes = %v, want %d", meta["input_bytes"], len("inbytes"))
	}
}

// TestFileSinkConcurrentSteps spins up N goroutines calling Step
// simultaneously and verifies N distinct subdirectories with
// monotonic numeric prefixes and N step.start + N step.end lines in
// timeline.jsonl.
func TestFileSinkConcurrentSteps(t *testing.T) {
	const N = 16
	s, dir := newSink(t, ModeFull)
	now := time.Now()
	tr := s.Begin(RequestInfo{RID: "concurrent", StartedAt: now, Payload: []byte(`{}`)})

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr.Step(StepInfo{
				Stack: "s", Scope: 100, Name: "op",
				Operation: "http://x", Transport: "http",
				Input:     []byte(`{}`),
				Output:    []byte(`{}`),
				StartedAt: now, FinishedAt: now,
				Status: "ok",
			})
		}(i)
	}
	wg.Wait()
	tr.End("ok", []byte(`{}`))

	entries, _ := os.ReadDir(filepath.Join(dir, "requests", "concurrent", "steps"))
	if len(entries) != N {
		t.Errorf("steps count = %d, want %d", len(entries), N)
	}

	tlb, _ := os.ReadFile(filepath.Join(dir, "requests", "concurrent", "timeline.jsonl"))
	starts := strings.Count(string(tlb), `"event":"step.start"`)
	ends := strings.Count(string(tlb), `"event":"step.end"`)
	if starts != N || ends != N {
		t.Errorf("step.start=%d step.end=%d, want %d each", starts, ends, N)
	}
}

// TestFileSinkWritesAreAtomic asserts the disk never contains a
// `.tmp-*` intermediate file after a successful Step or End. If a
// reader were browsing /traces/ mid-request, no half-written
// artifact would be visible.
func TestFileSinkWritesAreAtomic(t *testing.T) {
	s, dir := newSink(t, ModeFull)
	now := time.Now()
	tr := s.Begin(RequestInfo{RID: "atomic", StartedAt: now, Payload: []byte(`{"big":"req"}`)})
	tr.Step(StepInfo{
		Stack: "s", Scope: 100, Name: "op",
		Operation: "http://x", Transport: "http",
		Input:     []byte(`{"in":1}`),
		Output:    []byte(`{"out":1}`),
		StartedAt: now, FinishedAt: now,
		Status: "ok",
	})
	tr.End("ok", []byte(`{"final":true}`))

	// Walk the entire request dir and assert no .tmp-* files remain
	// AND every JSON file parses cleanly (atomic rename never published
	// a partial file).
	root := filepath.Join(dir, "requests", "atomic")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), ".tmp-") {
			t.Errorf("stale temp file: %s", p)
		}
		if strings.HasSuffix(info.Name(), ".json") {
			b, _ := os.ReadFile(p)
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Errorf("%s: not valid JSON: %v (%q)", p, err, b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestRequestUsageTenantOverride: every request enters pinned to `_sys`, so
// in.json always records `_sys`; the resolved tenant rides the request.usage
// event and must override it in both Get and the list Summary — that's what
// admin tenant-scoping filters on.
func TestRequestUsageTenantOverride(t *testing.T) {
	s, dir := newSink(t, ModeSummary)
	start := time.Now()
	tr := s.Begin(RequestInfo{
		RID:       "r-routed",
		Src:       "http",
		Tenant:    "_sys", // entry tenant
		Stack:     "boot/0",
		StartedAt: start,
		Payload:   []byte(`{"_txc":{"tenant":"_sys"}}`),
	})
	tr.Event(TimelineEvent{
		Ts:     start.Add(time.Millisecond),
		Event:  "request.usage",
		Fields: map[string]any{"tenant": "prod-mankins"},
	})
	tr.End("ok", []byte(`{"ok":true}`))

	rdr := &fileReader{dir: dir}
	d, err := rdr.Get(context.Background(), "r-routed", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Tenant != "prod-mankins" {
		t.Errorf("Get tenant = %q, want prod-mankins (resolved overrides _sys)", d.Tenant)
	}
	res, err := rdr.List(context.Background(), ListQuery{Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Traces) != 1 || res.Traces[0].Tenant != "prod-mankins" {
		t.Errorf("List summary tenant = %+v, want prod-mankins", res.Traces)
	}
}

// TestListTenantFilter: q.Tenant restricts the list to traces resolved to that
// tenant (the leak fix); the unfiltered list still sees everything.
func TestListTenantFilter(t *testing.T) {
	s, dir := newSink(t, ModeSummary)
	seed := func(rid, tenant string) {
		tr := s.Begin(RequestInfo{RID: rid, Src: "http", Tenant: "_sys", Stack: "boot/0", StartedAt: time.Now(), Payload: []byte(`{}`)})
		tr.Event(TimelineEvent{Ts: time.Now(), Event: "request.usage", Fields: map[string]any{"tenant": tenant}})
		tr.End("ok", []byte(`{}`))
	}
	seed("r-a", "tenant-a")
	seed("r-b", "tenant-b")
	seed("r-sys", "_sys")

	rdr := &fileReader{dir: dir}
	scoped, err := rdr.List(context.Background(), ListQuery{Limit: 50, Tenant: "tenant-a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if scoped.Total != 1 {
		t.Errorf("scoped Total = %d, want 1", scoped.Total)
	}
	for _, su := range scoped.Traces {
		if su.Tenant != "tenant-a" {
			t.Errorf("scoped list leaked %s (tenant %s)", su.RID, su.Tenant)
		}
	}
	all, err := rdr.List(context.Background(), ListQuery{Limit: 50})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if all.Total != 3 {
		t.Errorf("unfiltered Total = %d, want 3", all.Total)
	}
	// Tenant scoping must vary the ETag so caches don't cross tenants.
	if scoped.ETag == all.ETag {
		t.Errorf("scoped + unfiltered share ETag %q", scoped.ETag)
	}
}

// TestRequestUsageRoundTrip asserts the request.usage timeline event the
// chassis emits at convergence is parsed back into RequestDetail.Fuel and
// BytesOut by the file reader. Uses ModeSummary to prove the value survives
// even when out.json isn't written (the timeline carries it in every mode).
func TestRequestUsageRoundTrip(t *testing.T) {
	s, dir := newSink(t, ModeSummary)
	start := time.Now()
	tr := s.Begin(RequestInfo{
		RID:       "req-usage",
		Src:       "http",
		Stack:     "boot/0",
		StartedAt: start,
		Payload:   []byte(`{"_txc":{"src":"http"}}`),
	})
	tr.Event(TimelineEvent{
		Ts:     start.Add(time.Millisecond),
		Event:  "request.usage",
		Fields: map[string]any{"fuel": int64(105), "bytes_out": 42},
	})
	tr.End("ok", []byte(`{"ok":true}`))

	rdr := &fileReader{dir: dir}
	d, err := rdr.Get(context.Background(), "req-usage", false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Fuel != 105 {
		t.Errorf("Fuel=%d, want 105", d.Fuel)
	}
	if d.BytesOut != 42 {
		t.Errorf("BytesOut=%d, want 42", d.BytesOut)
	}
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSanitizeNameNeutralizesDotSegments(t *testing.T) {
	for in, want := range map[string]string{
		"":            "unnamed",
		".":           "_.",
		"..":          "_..",
		"...":         "_...",
		"op://HELLO":  "op___HELLO",
		"in.json":     "in.json",
		"website/100": "website_100",
	} {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
