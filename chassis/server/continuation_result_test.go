package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
)

// envIn builds the op input envelope continuationResultBody reads:
// _txc.continuation (+ optional ?format=json in the query map).
func envIn(t *testing.T, rcid, format string) []byte {
	t.Helper()
	in := "{}"
	in, _ = sjson.Set(in, "_txc.continuation", rcid)
	if format != "" {
		in, _ = sjson.Set(in, "_txc.web.req.url.query.format.0", format)
	}
	return []byte(in)
}

func decodeBody(t *testing.T, env string) []byte {
	t.Helper()
	b64 := gjson.Get(env, "_txc.web.res.body").String()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("body not base64: %v (%s)", err, env)
	}
	return b
}

func newRuns(t *testing.T) *continuation.Runs {
	t.Helper()
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	return continuation.NewRuns(fs)
}

func TestContinuationResultUnknown(t *testing.T) {
	env := continuationResultBody(context.Background(), newRuns(t), 0, envIn(t, "rc_nope", ""))
	if got := gjson.Get(env, "_txc.web.res.status").Int(); got != 404 {
		t.Fatalf("status=%d want 404; env=%s", got, env)
	}
	// Body is base64-encoded JSON; decode and assert it carries
	// the rcid + a structured reason (admins can grep on either).
	bodyB64 := gjson.Get(env, "_txc.web.res.body").String()
	body, _ := base64.StdEncoding.DecodeString(bodyB64)
	if got := gjson.GetBytes(body, "rcid").String(); got != "rc_nope" {
		t.Errorf("404 body missing rcid; got %q (body=%s)", got, body)
	}
	if got := gjson.GetBytes(body, "reason").String(); got != "unknown_or_expired" {
		t.Errorf("404 body reason=%q want unknown_or_expired (well-formed rcid that doesn't resolve)", got)
	}
}

// TestContinuationResultMalformedRcid — a value that doesn't match
// the `rc_…` shape is flagged distinctly so log grep can separate
// client-side typos / probe traffic from genuinely-expired runs.
func TestContinuationResultMalformedRcid(t *testing.T) {
	env := continuationResultBody(context.Background(), newRuns(t), 0, envIn(t, "totally-wrong", ""))
	bodyB64 := gjson.Get(env, "_txc.web.res.body").String()
	body, _ := base64.StdEncoding.DecodeString(bodyB64)
	if got := gjson.GetBytes(body, "reason").String(); got != "malformed_rcid" {
		t.Errorf("404 reason=%q want malformed_rcid for an rcid without rc_ prefix", got)
	}
}

// TestContinuationResultEmptyRcid — no rcid at all → a different
// reason so we don't conflate "missing" with "doesn't resolve".
func TestContinuationResultEmptyRcid(t *testing.T) {
	env := continuationResultBody(context.Background(), newRuns(t), 0, envIn(t, "", ""))
	bodyB64 := gjson.Get(env, "_txc.web.res.body").String()
	body, _ := base64.StdEncoding.DecodeString(bodyB64)
	if got := gjson.GetBytes(body, "reason").String(); got != "empty_or_missing_rcid" {
		t.Errorf("404 reason=%q want empty_or_missing_rcid", got)
	}
}

func TestContinuationResultWaiting(t *testing.T) {
	ctx := context.Background()
	runs := newRuns(t)
	runID, rcid, err := runs.CreateRun(ctx, "tnt", "site", "", "site/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	manifest := []continuation.OpManifestEntry{
		{Ordinal: 0, Op: "a", Async: true},
		{Ordinal: 1, Op: "b", Async: true},
	}
	if err := runs.SuspendStage(ctx, runID, "site/100", `{}`, "", manifest); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}

	env := continuationResultBody(ctx, runs, 0, envIn(t, rcid, ""))
	if got := gjson.Get(env, "_txc.web.res.status").Int(); got != 202 {
		t.Fatalf("status=%d want 202", got)
	}
	if ra := gjson.Get(env, "_txc.web.res.headers.retry-after.0").String(); ra == "" {
		t.Fatalf("missing Retry-After; env=%s", env)
	}
	if s := gjson.GetBytes(decodeBody(t, env), "status").String(); s != continuation.StateWaiting {
		t.Fatalf("body status=%q want waiting", s)
	}
}

func TestContinuationResultCompletedPageAndJSON(t *testing.T) {
	ctx := context.Background()
	runs := newRuns(t)
	runID, rcid, err := runs.CreateRun(ctx, "tnt", "site", "", "site/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	htmlB64 := base64.StdEncoding.EncodeToString([]byte("<h1>hello cruel world</h1>"))
	result := `{"words":["a"],"_txc":{"tenant":"someoneelse","web":{"res":{"status":200,"headers":{"content-type":["text/html; charset=utf-8"]},"body":"` + htmlB64 + `"}}}}`
	if err := runs.WriteResult(ctx, runID, []byte(result)); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}

	// Default → deferred page: ONLY the stored _txc.web.res is surfaced
	// (not the run's other fields / its _txc.tenant).
	page := continuationResultBody(ctx, runs, 0, envIn(t, rcid, ""))
	if got := gjson.Get(page, "_txc.web.res.status").Int(); got != 200 {
		t.Fatalf("page status=%d want 200", got)
	}
	if ct := gjson.Get(page, "_txc.web.res.headers.content-type.0").String(); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}
	if string(decodeBody(t, page)) != "<h1>hello cruel world</h1>" {
		t.Fatalf("page body=%q", decodeBody(t, page))
	}
	if gjson.Get(page, "words").Exists() || gjson.Get(page, "_txc.tenant").Exists() {
		t.Fatalf("page leaked run fields/tenant: %s", page)
	}

	// ?format=json → status doc with raw result embedded.
	js := continuationResultBody(ctx, runs, 0, envIn(t, rcid, "json"))
	if got := gjson.Get(js, "_txc.web.res.status").Int(); got != 200 {
		t.Fatalf("json status=%d want 200", got)
	}
	doc := decodeBody(t, js)
	var d map[string]any
	if err := json.Unmarshal(doc, &d); err != nil {
		t.Fatalf("json body not JSON: %v (%s)", err, doc)
	}
	if d["status"] != "completed" || d["continuation"] != rcid || d["result"] == nil {
		t.Fatalf("json doc=%v", d)
	}
}

func TestContinuationResultFailed(t *testing.T) {
	ctx := context.Background()
	runs := newRuns(t)
	runID, rcid, err := runs.CreateRun(ctx, "tnt", "site", "", "site/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := runs.FailRun(ctx, runID, "boom"); err != nil {
		t.Fatalf("FailRun: %v", err)
	}
	env := continuationResultBody(ctx, runs, 0, envIn(t, rcid, ""))
	if got := gjson.Get(env, "_txc.web.res.status").Int(); got != 502 {
		t.Fatalf("status=%d want 502; env=%s", got, env)
	}
}

// waitIn builds a poll input with an Accept header (and optional
// ?format=json) so the browser-vs-machine negotiation can be exercised.
func waitIn(t *testing.T, rcid, accept, format string) []byte {
	t.Helper()
	in := "{}"
	in, _ = sjson.Set(in, "_txc.continuation", rcid)
	if accept != "" {
		in, _ = sjson.Set(in, "_txc.web.req.headers.Accept.0", accept)
	}
	if format != "" {
		in, _ = sjson.Set(in, "_txc.web.req.url.query.format.0", format)
	}
	return []byte(in)
}

func newWaitingRun(t *testing.T, runs *continuation.Runs) string {
	t.Helper()
	ctx := context.Background()
	runID, rcid, err := runs.CreateRun(ctx, "tnt", "site", "", "site/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := runs.SuspendStage(ctx, runID, "site/100", `{}`, "",
		[]continuation.OpManifestEntry{{Ordinal: 0, Op: "a", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	return rcid
}

// A browser (Accept: text/html) waiting on a run gets the branded HTML
// page; curl/fetch/?format=json get the JSON 202. No Refresh header in
// any case (the page owns refresh; a server Refresh would fight the JS
// poller).
func TestContinuationResultWaitingBrowserVsMachine(t *testing.T) {
	ctx := context.Background()
	runs := newRuns(t)
	rcid := newWaitingRun(t, runs)

	const acceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	// Browser → HTML waiting page.
	page := continuationResultBody(ctx, runs, 0, waitIn(t, rcid, acceptHTML, ""))
	if got := gjson.Get(page, "_txc.web.res.status").Int(); got != 202 {
		t.Fatalf("html status=%d want 202", got)
	}
	if ct := gjson.Get(page, "_txc.web.res.headers.content-type.0").String(); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q want text/html; charset=utf-8", ct)
	}
	if cc := gjson.Get(page, "_txc.web.res.headers.cache-control.0").String(); cc != "no-store" {
		t.Fatalf("cache-control=%q want no-store", cc)
	}
	if ra := gjson.Get(page, "_txc.web.res.headers.retry-after.0").String(); ra != "3" {
		t.Fatalf("retry-after=%q want 3", ra)
	}
	if gjson.Get(page, "_txc.web.res.headers.refresh.0").Exists() {
		t.Fatalf("must NOT set a Refresh header (page owns refresh): %s", page)
	}
	body := decodeBody(t, page)
	if !strings.Contains(string(body), "thanks, c") {
		t.Fatalf("html body not the branded page: %.120s", body)
	}

	// Browser but ?format=json → JSON 202 (programmatic override).
	js := continuationResultBody(ctx, runs, 0, waitIn(t, rcid, acceptHTML, "json"))
	if ct := gjson.Get(js, "_txc.web.res.headers.content-type.0").String(); ct != "application/json" {
		t.Fatalf("format=json content-type=%q want application/json", ct)
	}

	// Machine (Accept: application/json) → JSON 202.
	m := continuationResultBody(ctx, runs, 0, waitIn(t, rcid, "application/json", ""))
	if ct := gjson.Get(m, "_txc.web.res.headers.content-type.0").String(); ct != "application/json" {
		t.Fatalf("machine content-type=%q want application/json", ct)
	}
	if gjson.Get(m, "_txc.web.res.headers.refresh.0").Exists() {
		t.Fatalf("JSON path must not set Refresh")
	}
}

// newWaitingRunIDs is newWaitingRun but also returns runID so the test
// can drive the run terminal from a goroutine while a poll is held.
func newWaitingRunIDs(t *testing.T, runs *continuation.Runs) (runID, rcid string) {
	t.Helper()
	ctx := context.Background()
	runID, rcid, err := runs.CreateRun(ctx, "tnt", "site", "", "site/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := runs.SuspendStage(ctx, runID, "site/100", `{}`, "",
		[]continuation.OpManifestEntry{{Ordinal: 0, Op: "a", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	return runID, rcid
}

// With long-poll enabled, a JSON poll on a waiting run is held open and
// returns as soon as the run completes — well before the budget — not
// on a fixed client grid.
func TestContinuationResultLongPollEarlyReturn(t *testing.T) {
	runs := newRuns(t)
	runID, rcid := newWaitingRunIDs(t, runs)

	result := `{"_txc":{"web":{"res":{"status":200,"body":""}}}}`
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if err := runs.WriteResult(context.Background(), runID, []byte(result)); err != nil {
			t.Errorf("WriteResult: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	start := time.Now()
	js := continuationResultBody(ctx, runs, 12000, envIn(t, rcid, "json"))
	elapsed := time.Since(start)

	if got := gjson.Get(js, "_txc.web.res.status").Int(); got != 200 {
		t.Fatalf("status=%d want 200 (completed); env=%s", got, js)
	}
	if s := gjson.GetBytes(decodeBody(t, js), "status").String(); s != "completed" {
		t.Fatalf("body status=%q want completed", s)
	}
	// Detected on a ~1s tick after the 1.5s write — comfortably under
	// the 12s budget, and not instant.
	if elapsed < 1*time.Second || elapsed > 5*time.Second {
		t.Fatalf("elapsed=%v want ~2s (early return, not budget/instant)", elapsed)
	}
}

// Long-poll budget is bounded by the request deadline minus headroom:
// a run that stays waiting returns a clean 202 before the context dies,
// not at the (much larger) configured cap and not hanging.
func TestContinuationResultLongPollBudgetBoundedByDeadline(t *testing.T) {
	runs := newRuns(t)
	_, rcid := newWaitingRunIDs(t, runs)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	env := continuationResultBody(ctx, runs, 60000, envIn(t, rcid, "json"))
	elapsed := time.Since(start)

	if got := gjson.Get(env, "_txc.web.res.status").Int(); got != 202 {
		t.Fatalf("status=%d want 202; env=%s", got, env)
	}
	if s := gjson.GetBytes(decodeBody(t, env), "status").String(); s != continuation.StateWaiting {
		t.Fatalf("body status=%q want waiting", s)
	}
	// Budget = ctxDeadline(2s) - 1.5s headroom ≈ 0.5s; must return well
	// before the 2s context deadline (and nowhere near the 60s cap).
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("elapsed=%v want <1.5s (deadline-bounded, returned before ctx death)", elapsed)
	}
}

// longPollMS == 0 is the rollback switch: a JSON poll on a waiting run
// answers 202 immediately, exactly as the pre-long-poll handler did.
func TestContinuationResultLongPollDisabled(t *testing.T) {
	runs := newRuns(t)
	_, rcid := newWaitingRunIDs(t, runs)

	start := time.Now()
	env := continuationResultBody(context.Background(), runs, 0, envIn(t, rcid, "json"))
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed=%v want immediate (single-shot)", elapsed)
	}
	if got := gjson.Get(env, "_txc.web.res.status").Int(); got != 202 {
		t.Fatalf("status=%d want 202", got)
	}
}
