package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// captureHandler mounts a txco:// handler that records the exact bytes it
// received and echoes an empty JSON object (so nothing merges back and the
// captured input stays the only observable). Returns the capture slot.
func captureHandler(t *testing.T, pu *Unit, scheme string) *atomic.Value {
	t.Helper()
	var got atomic.Value
	pu.Handle([]byte(scheme),
		event.OpsHandlerFunc(func(_ context.Context, _ string, in, _ []byte) (event.Payload, error) {
			got.Store(string(in))
			return event.Payload{Raw: "{}", Type: event.JSON}, nil
		}))
	return &got
}

// TestNoSelectDispatchByteIdentical — ORACLE: a rule with no SELECT
// dispatches the scope envelope byte-identically to the inlet input plus
// the runtime identity stamp (_txc.op, _txc.step) applied by
// injectRuntimeIdentity. This freezes the no-SELECT wire contract so the
// SELECT-projection change cannot disturb rules that don't use SELECT.
func TestNoSelectDispatchByteIdentical(t *testing.T) {
	pu, _ := newTestUnit(t)
	got := captureHandler(t, pu, "txco://test-capture")

	seedRule(t, pu, "oracle/plain", "plain", `EXEC "txco://test-capture"`)

	in := `{"_ts":"2026-08-21T00:00:00Z","_txc":{"route":{"stack":"oracle/plain"}},"payload":{"q":"hi","n":1},"noise":true}`
	runOne(t, pu, in, "oracle/plain/0")

	captured, _ := got.Load().(string)
	if captured == "" {
		t.Fatal("handler never invoked")
	}
	want := injectRuntimeIdentity(in, "oracle/plain", "plain", 0)
	if captured != want {
		t.Errorf("dispatch bytes changed for a no-SELECT rule:\n got: %s\nwant: %s", captured, want)
	}
	// Belt and braces: identity landed where expected.
	if gjson.Get(captured, "_txc.op").String() != "oracle/plain/plain" {
		t.Errorf("_txc.op = %q", gjson.Get(captured, "_txc.op").String())
	}
}

// projectedKeys returns the sorted top-level keys of a JSON document.
func projectedKeys(t *testing.T, doc string) []string {
	t.Helper()
	var keys []string
	gjson.Parse(doc).ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	sort.Strings(keys)
	return keys
}

// TestSelectProjectionWireBody — the core acceptance: a SELECT'd rule
// dispatches ONLY the assigned destinations plus envelope identity.
// `SELECT ._in.q AS .q` posts exactly {"q":…, "_ts":…, "_txc":{op,step}}.
func TestSelectProjectionWireBody(t *testing.T) {
	pu, _ := newTestUnit(t)
	got := captureHandler(t, pu, "txco://test-capture")

	seedRule(t, pu, "proj/wire", "wire",
		`SELECT ._in.q AS .q EXEC "txco://test-capture"`)

	in := `{"_ts":"2026-08-21T00:00:00Z","_in":{"q":"hi","x":1},"noise":true,"_txc":{"route":{"stack":"proj/wire"}}}`
	runOne(t, pu, in, "proj/wire/0")

	captured, _ := got.Load().(string)
	if captured == "" {
		t.Fatal("handler never invoked")
	}
	if want := []string{"_ts", "_txc", "q"}; !slices.Equal(projectedKeys(t, captured), want) {
		t.Errorf("projected top-level keys = %v, want %v; body=%s", projectedKeys(t, captured), want, captured)
	}
	var txcKeys []string
	gjson.Get(captured, "_txc").ForEach(func(k, _ gjson.Result) bool {
		txcKeys = append(txcKeys, k.String())
		return true
	})
	sort.Strings(txcKeys)
	if want := []string{"op", "step"}; !slices.Equal(txcKeys, want) {
		t.Errorf("_txc keys = %v, want identity only %v; body=%s", txcKeys, want, captured)
	}
	if got := gjson.Get(captured, "q").String(); got != "hi" {
		t.Errorf("q = %q, want hi", got)
	}
	if got := gjson.Get(captured, "_ts").String(); got != "2026-08-21T00:00:00Z" {
		t.Errorf("_ts = %q — projection must carry the inlet timestamp forward", got)
	}
}

// TestSelectProjectionHTTPWireBody — same acceptance over a real HTTP
// dispatch: the POSTed request body is the projected envelope.
func TestSelectProjectionHTTPWireBody(t *testing.T) {
	var gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody.Store(string(b))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	pu, _ := newTestUnit(t)
	pu.HTTPClient = srv.Client()
	seedRule(t, pu, "proj/http", "http",
		`SELECT ._in.q AS .q EXEC "`+srv.URL+`"`)

	runOne(t, pu, `{"_ts":"2026-08-21T00:00:00Z","_in":{"q":"hi"},"secretish":"do-not-ship"}`, "proj/http/0")

	body, _ := gotBody.Load().(string)
	if body == "" {
		t.Fatal("server never hit")
	}
	if want := []string{"_ts", "_txc", "q"}; !slices.Equal(projectedKeys(t, body), want) {
		t.Errorf("HTTP body keys = %v, want %v; body=%s", projectedKeys(t, body), want, body)
	}
	if gjson.Get(body, "secretish").Exists() {
		t.Errorf("unselected field shipped on the wire: %s", body)
	}
}

// TestSelectWithReadsUnselectedPath — WITH resolves against the FULL
// pre-projection input, so chassis directives may reference paths the
// projection drops from the wire view.
func TestSelectWithReadsUnselectedPath(t *testing.T) {
	pu, _ := newTestUnit(t)
	var gotMeta atomic.Value
	pu.Handle([]byte("txco://test-capture-meta"),
		event.OpsHandlerFunc(func(ctx context.Context, _ string, _, _ []byte) (event.Payload, error) {
			gotMeta.Store(operation.MetaFromContext(ctx))
			return event.Payload{Raw: "{}", Type: event.JSON}, nil
		}))

	seedRule(t, pu, "proj/with", "with",
		`SELECT ._in.q AS .q WITH note = ._in.other EXEC "txco://test-capture-meta"`)

	runOne(t, pu, `{"_in":{"q":"hi","other":"full-view"}}`, "proj/with/0")

	meta, _ := gotMeta.Load().(string)
	if got := gjson.Get(meta, "note").String(); got != "full-view" {
		t.Errorf("WITH resolved against projected view; note = %q, want full-view (meta=%s)", got, meta)
	}
}

// TestSelectEmitTTLClampUsesFullEnv — the EMIT `_txc.ttl` clamp reads
// the envelope's current TTL off the FULL view. A projected env would
// read 0 and clamp every requested TTL to zero.
func TestSelectEmitTTLClampUsesFullEnv(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "proj/ttl", "ttl",
		`SELECT ._in.q AS .q EMIT @ttl = 5`)

	// Inlet TTL 3 is decremented to 2 by the hop machinery before the
	// op runs; the clamp must read that live value off the FULL env
	// (a projected env reads 0 and clamps the requested 5 to zero).
	p := runOne(t, pu, `{"_txc":{"ttl":3},"_in":{"q":"hi"}}`, "proj/ttl/0")
	if got := gjson.Get(p.Raw, "_txc.ttl").Int(); got != 2 {
		t.Errorf("_txc.ttl = %d, want 2 (raise refused, clamp against full env); body=%s", got, p.Raw)
	}
}

// TestSelectEmitReadsUnselectedPath — user EMIT value expressions
// resolve against the FULL envelope (via MultiEnv fallback), so
// `EMIT .copy = ._in.other` works even when `._in.other` was not
// selected onto the wire view.
func TestSelectEmitReadsUnselectedPath(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "proj/emit", "emit",
		`SELECT ._in.q AS .q EMIT .copy = ._in.other`)

	p := runOne(t, pu, `{"_in":{"q":"hi","other":"kept"}}`, "proj/emit/0")
	if got := gjson.Get(p.Raw, "copy").String(); got != "kept" {
		t.Errorf("EMIT resolved against projected view; copy = %q, want kept; body=%s", got, p.Raw)
	}
}

// TestSelectMocksStillApply — caller-driven `_txc.mocks` pattern
// interception reads the FULL envelope view; a SELECT'd rule must not
// silently lose mocking because the pattern field wasn't selected.
func TestSelectMocksStillApply(t *testing.T) {
	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		"proj/mock", 0, "mock",
		`SELECT ._in.q AS .q EXEC "https://unreachable.invalid/never"`,
		`{"mocked":true}`); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	p := runOne(t, pu, `{"_txc":{"mocks":["proj/mock/**"]},"_in":{"q":"hi"}}`, "proj/mock/0")
	if !gjson.Get(p.Raw, "mocked").Bool() {
		t.Errorf("mock did not intercept for SELECT'd rule; body=%s", p.Raw)
	}
}

// TestSelectParallelNoExecRulesPersist — the mcp-quickstart / demo
// curriculum shape: two no-EXEC SELECT rules at the same scope. Both
// synthetic-EMIT contributions merge (additively) into the scope
// envelope alongside the original fields.
func TestSelectParallelNoExecRulesPersist(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedRule(t, pu, "proj/par", "repo",
		`SELECT @web.req.url.query.repoName.0 AS .repoName DEFAULT "facebook/react"`)
	seedRule(t, pu, "proj/par", "question",
		`SELECT @web.req.url.query.question.0 AS .question DEFAULT "What is jsx used for?"`)

	p := runOne(t, pu, `{"marker":true}`, "proj/par/0")
	if got := gjson.Get(p.Raw, "repoName").String(); got != "facebook/react" {
		t.Errorf("repoName = %q; body=%s", got, p.Raw)
	}
	if got := gjson.Get(p.Raw, "question").String(); got != "What is jsx used for?" {
		t.Errorf("question = %q; body=%s", got, p.Raw)
	}
	if !gjson.Get(p.Raw, "marker").Bool() {
		t.Errorf("original envelope field lost — persistence must stay additive; body=%s", p.Raw)
	}
}

// TestSelectStructuredValueOnWire — SetRaw fidelity: objects keep their
// JSON shape through the projection, and a DEFAULT array lands typed.
func TestSelectStructuredValueOnWire(t *testing.T) {
	pu, _ := newTestUnit(t)
	got := captureHandler(t, pu, "txco://test-capture-structured")

	seedRule(t, pu, "proj/shape", "shape",
		`SELECT ._in.obj AS .obj, ._in.missing AS .arr DEFAULT ["a", "b"] EXEC "txco://test-capture-structured"`)

	runOne(t, pu, `{"_in":{"obj":{"k":[1,2],"s":"v"}}}`, "proj/shape/0")

	captured, _ := got.Load().(string)
	if captured == "" {
		t.Fatal("handler never invoked")
	}
	if got := gjson.Get(captured, "obj.k.1").Int(); got != 2 {
		t.Errorf("structured value lost shape; obj = %s", gjson.Get(captured, "obj").Raw)
	}
	if got := gjson.Get(captured, "arr.1").String(); got != "b" {
		t.Errorf("DEFAULT array mangled; arr = %s", gjson.Get(captured, "arr").Raw)
	}
}

// stepRecorder is a minimal RequestTracer that records StepInfo values.
type stepRecorder struct {
	mu    sync.Mutex
	steps []trace.StepInfo
}

func (r *stepRecorder) Step(info trace.StepInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, info)
}
func (r *stepRecorder) Event(trace.TimelineEvent) {}
func (r *stepRecorder) End(_, _ string, _ []byte) {}

// TestSelectTraceShowsProjectedInput — the trace records what the op
// actually received: StepInfo.Input for a SELECT'd op is the projected
// view, not the full envelope.
func TestSelectTraceShowsProjectedInput(t *testing.T) {
	pu, _ := newTestUnit(t)
	captureHandler(t, pu, "txco://test-capture-trace")
	seedRule(t, pu, "proj/trace", "trace",
		`SELECT ._in.q AS .q EXEC "txco://test-capture-trace"`)

	rec := &stepRecorder{}
	ctx := trace.WithContext(context.Background(), rec)
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(ctx, `{"_in":{"q":"hi"},"noise":true}`, "proj/trace/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-resCh

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var found bool
	for _, st := range rec.steps {
		if st.Operation != "txco://test-capture-trace" {
			continue
		}
		found = true
		in := string(st.Input)
		if gjson.Get(in, "noise").Exists() {
			t.Errorf("trace Input shows unprojected envelope: %s", in)
		}
		if gjson.Get(in, "q").String() != "hi" {
			t.Errorf("trace Input missing projected field: %s", in)
		}
	}
	if !found {
		t.Fatalf("no step recorded for the capture op; steps=%d", len(rec.steps))
	}
}

// TestExtractMCPArgumentsProjectedEnvelope — a projected envelope has no
// `_txc.web.req.body`, so MCP argument extraction takes the
// strip-identity path and yields exactly the selection.
func TestExtractMCPArgumentsProjectedEnvelope(t *testing.T) {
	projected := `{"q":"hi","_ts":"2026-08-21T00:00:00Z","_txc":{"op":"s/n","step":0}}`
	args := extractMCPArguments(projected)
	if want := []string{"q"}; !slices.Equal(projectedKeys(t, args), want) {
		t.Errorf("mcp args keys = %v, want %v; args=%s", projectedKeys(t, args), want, args)
	}
	if got := gjson.Get(args, "q").String(); got != "hi" {
		t.Errorf("q = %q, want hi", got)
	}
}
