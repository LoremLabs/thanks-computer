package processor

// Follow-up acceptance tests for the continuable-metadata work:
//
//   - local-async (mcp+http mode="async") adopts the detached-ctx helpers:
//     WITH secrets materialize, and the terminal carries transport + drawn
//     fuel that Resume folds into the envelope meter.
//   - a resume segment that ends by RE-SUSPENDING bills its fuel delta
//     (middle segments of a multi-suspend run were previously unbilled).
//   - a continuable promotion on an LMTP-driven request with no pre-emitted
//     verdict synthesizes a 250 accept (durable suspend IS acceptance);
//     a pre-emitted verdict always wins.
//   - join_at_scope on a continuable op is rejected loudly, not silently
//     ignored.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// --- local-async: fuel + transport on the terminal -------------------------

func TestLocalAsyncDetachedFuelAndTransport(t *testing.T) {
	pu := newContinuableUnit(t)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"fueled"}]}}`)
	}

	seedOp(t, pu, "acme", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	ctx := context.Background()
	term, err := pu.Runs.ReadOpTerminal(ctx, runID, "acme/100", 0, "ask")
	if err != nil {
		t.Fatalf("ReadOpTerminal: %v", err)
	}
	if term.Transport != "mcp+http" {
		t.Errorf("terminal transport = %q, want mcp+http", term.Transport)
	}
	if term.FuelUsed < 25 {
		t.Errorf("terminal fuel_used = %d, want >= 25 (detached exec charge)", term.FuelUsed)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, runID)
	if !ok {
		t.Fatal("no result.json")
	}
	ss, _ := pu.Runs.ReadStageSuspended(ctx, runID, "acme/100")
	if got := gjson.GetBytes(res, "_txc.fuel_used").Int(); got < FuelUsedFromEnvelope(ss.ScopeEnvelope)+term.FuelUsed {
		t.Errorf("result fuel = %d, want >= suspend + detached (%d + %d)",
			got, FuelUsedFromEnvelope(ss.ScopeEnvelope), term.FuelUsed)
	}
}

// --- local-async: WITH secrets materialize before the detach ---------------

func TestLocalAsyncSecretsMaterialize(t *testing.T) {
	pu := newContinuableUnit(t)
	const cleartext = "sk_live_localasync_xyz"
	setupTenantSecretStore(t, pu, "MCP_API_KEY", cleartext)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"authed"}]}}`)
	}

	seedTenantOp(t, pu, "e2e/lasec", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s", `+
			`secrets.headers.authorization.secret = "MCP_API_KEY", `+
			`secrets.headers.authorization.format = "Bearer {}"`)

	resCh := make(chan event.Payload, 1)
	go func() {
		_ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "e2e/lasec/100", resCh)
	}()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls) == 0 {
		t.Fatal("MCP stub saw no requests")
	}
	last := stub.calls[len(stub.calls)-1] // tools/call carries the overlay
	if got := last.Header.Get("Authorization"); got != "Bearer "+cleartext {
		t.Errorf("tools/call Authorization = %q, want %q (WITH secrets never materialized on local-async?)",
			got, "Bearer "+cleartext)
	}
}

// --- middle-segment billing on a multi-suspend chain -----------------------

func TestResumeSegmentUsageBilledOnChain(t *testing.T) {
	pu := newContinuableUnit(t)
	us := newChanUsage()
	pu.Usage = us

	stubA, _ := delayedStub(t, 400*time.Millisecond, `{"first":"a"}`)
	stubB, _ := delayedStub(t, 400*time.Millisecond, `{"second":"b"}`)

	seedOp(t, pu, "acme", 100, "one",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stubA.URL))
	seedOp(t, pu, "acme", 200, "two",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stubB.URL))
	seedOp(t, pu, "acme", 300, "done", `EMIT .done = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("final state = %q, want completed", st)
	}

	// Two billing lines: the MIDDLE segment (resume of acme/100 ended by the
	// re-suspend at acme/200) and the FINAL segment (resume of acme/200 ran
	// to completion). Their deltas must tile the run's post-suspend fuel.
	events := map[string]usage.UsageEvent{}
	for range 2 {
		select {
		case ev := <-us.ch:
			events[ev.RID] = ev
		case <-time.After(5 * time.Second):
			t.Fatalf("expected 2 resume usage events, got %d: %v", len(events), events)
		}
	}
	midRID := continuation.ResumeTraceRID(runID, "acme/100")
	finRID := continuation.ResumeTraceRID(runID, "acme/200")
	mid, ok := events[midRID]
	if !ok {
		t.Fatalf("no usage line for the middle segment (%s); got %v", midRID, events)
	}
	fin, ok := events[finRID]
	if !ok {
		t.Fatalf("no usage line for the final segment (%s); got %v", finRID, events)
	}
	if mid.Fuel <= 0 {
		t.Errorf("middle segment fuel = %d, want > 0 (was previously unbilled)", mid.Fuel)
	}
	if !mid.Billable || mid.Src != "continuation" {
		t.Errorf("middle segment misattributed: %+v", mid)
	}

	res, ok2, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok2 {
		t.Fatal("no result.json")
	}
	ss1, _ := pu.Runs.ReadStageSuspended(context.Background(), runID, "acme/100")
	wantTotal := FuelUsedFromEnvelope(string(res)) - FuelUsedFromEnvelope(ss1.ScopeEnvelope)
	if got := mid.Fuel + fin.Fuel; got != wantTotal {
		t.Errorf("segment deltas sum = %d, want %d (final − first suspend); mid=%d fin=%d",
			got, wantTotal, mid.Fuel, fin.Fuel)
	}
}

// --- LMTP: promotion with no verdict synthesizes 250 -----------------------

func TestContinuablePromotionLMTPAutoAccept(t *testing.T) {
	pu := newContinuableUnit(t)

	stub, _ := delayedStub(t, 400*time.Millisecond, `{"upstream":"late"}`)
	seedOp(t, pu, "mail", 100, "decide",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stub.URL))

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"src":"lmtp"}}`, "mail/100", resCh) }()

	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.lmtp.res.code").Int(); got != 250 {
			t.Errorf("lmtp.res.code = %d, want synthesized 250 (durable suspend = acceptance); payload=%s", got, p.Raw)
		}
		if gjson.Get(p.Raw, "_txc.lmtp.res.msg").String() == "" {
			t.Error("synthesized verdict missing msg")
		}
		drainContinuableRun(t, pu, p.Raw)
	case <-time.After(5 * time.Second):
		t.Fatal("no promotion payload within 5s")
	}
}

// drainContinuableRun waits for the promoted run behind a continuation
// payload to complete, so the detached goroutine isn't still writing store
// files while t.TempDir cleanup removes them.
func drainContinuableRun(t *testing.T, pu *Unit, payload string) {
	t.Helper()
	loc := gjson.Get(payload, "_txc.web.res.headers.location.0").String()
	rcid := strings.TrimPrefix(loc, "/?_txc.continuation=")
	if rcid == "" || rcid == loc {
		t.Fatalf("no continuation id in payload location %q", loc)
	}
	runID := resolveRunIDFromRcid(t, pu, rcid)
	_ = waitForRunCompleted(t, pu, runID)
}

// --- LMTP: a pre-emitted verdict is never overwritten ----------------------

func TestContinuablePromotionLMTPPreEmittedVerdictWins(t *testing.T) {
	pu := newContinuableUnit(t)

	stub, _ := delayedStub(t, 400*time.Millisecond, `{"upstream":"late"}`)
	seedOp(t, pu, "mail", 50, "verdict", `EMIT @lmtp.res.code = 251`)
	seedOp(t, pu, "mail", 100, "decide",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stub.URL))

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"src":"lmtp"}}`, "mail/50", resCh) }()

	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.lmtp.res.code").Int(); got != 251 {
			t.Errorf("lmtp.res.code = %d, want pre-emitted 251 untouched; payload=%s", got, p.Raw)
		}
		if got := gjson.Get(p.Raw, "_txc.lmtp.res.msg").String(); got != "" {
			t.Errorf("msg = %q, want empty (synthesis must not fire when a verdict exists)", got)
		}
		drainContinuableRun(t, pu, p.Raw)
	case <-time.After(5 * time.Second):
		t.Fatal("no promotion payload within 5s")
	}
}

// --- join_at_scope + continuable: loud rejection ---------------------------

func TestContinuableJoinAtScopeRejected(t *testing.T) {
	pu := newContinuableUnit(t)

	stub, _ := delayedStub(t, 0, `{"x":1}`)
	seedOp(t, pu, "acme", 100, "research",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s", join_at_scope = 300`, stub.URL))

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	select {
	case p := <-resCh:
		if !gjsonContains(p.Raw, "join_at_scope") {
			t.Errorf("error should name join_at_scope; got %s", p.Raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no rejection payload within 2s")
	}
}
