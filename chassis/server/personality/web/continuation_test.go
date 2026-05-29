package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// seedTwoOpStage creates a run + a stage with two async ops and returns
// the run-continuation id plus the first op's (opc, token). With two ops
// the barrier never completes on a single callback, so the handler stops
// at "recorded" and never invokes Resume (no DB needed).
func seedTwoOpStage(t *testing.T, runs *continuation.Runs) (rcid, opc, token string) {
	t.Helper()
	ctx := context.Background()
	runID, rc, err := runs.CreateRun(ctx, "", "acme", "", "acme/100", "", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	manifest := []continuation.OpManifestEntry{
		{Ordinal: 0, Op: "a", Async: true},
		{Ordinal: 1, Op: "b", Async: true},
	}
	if err := runs.SuspendStage(ctx, runID, "acme/100", `{}`, "", manifest); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	tok, hash, err := continuation.MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	opcID, err := continuation.NewOpContinuationID()
	if err != nil {
		t.Fatalf("NewOpContinuationID: %v", err)
	}
	tok2, hash2, _ := continuation.MintToken()
	opcID2, _ := continuation.NewOpContinuationID()
	_ = tok2
	specs := []continuation.OpRecordSpec{
		{Ordinal: 0, Op: "a", Async: true, OpContinuationID: opcID, TokenHash: hash, Input: []byte(`{}`)},
		{Ordinal: 1, Op: "b", Async: true, OpContinuationID: opcID2, TokenHash: hash2, Input: []byte(`{}`)},
	}
	if err := runs.CreateOpRecords(ctx, runID, "acme/100", specs); err != nil {
		t.Fatalf("CreateOpRecords: %v", err)
	}
	return rc, opcID, tok
}

func webWith(runs *continuation.Runs) *WebController {
	pu := &processor.Unit{Logger: zap.NewNop(), Runs: runs}
	return NewController(context.Background(), pu, trace.NoopSink{})
}

func postComplete(t *testing.T, web *WebController, opc, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/_txc/continuations/op/"+opc+"/complete", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req = mux.SetURLVars(req, map[string]string{"opc": opc})
	rec := httptest.NewRecorder()
	web.handleContinuationComplete(rec, req)
	return rec
}

func TestCompleteUnknownOpc404(t *testing.T) {
	fs, _ := filestore.New(t.TempDir())
	web := webWith(continuation.NewRuns(fs))
	rec := postComplete(t, web, "opc_nope", "tok", `{"status":"complete"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

func TestCompleteBadToken401(t *testing.T) {
	fs, _ := filestore.New(t.TempDir())
	runs := continuation.NewRuns(fs)
	_, opc, _ := seedTwoOpStage(t, runs)
	web := webWith(runs)

	if rec := postComplete(t, web, opc, "wrong-token", `{"status":"complete"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token code=%d want 401", rec.Code)
	}
	if rec := postComplete(t, web, opc, "", `{"status":"complete"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token code=%d want 401", rec.Code)
	}
}

func TestCompleteRecordsAndIsIdempotent(t *testing.T) {
	fs, _ := filestore.New(t.TempDir())
	runs := continuation.NewRuns(fs)
	rcid, opc, token := seedTwoOpStage(t, runs)
	web := webWith(runs)

	// First valid completion of op "a" (op "b" still pending → barrier
	// not satisfied → handler stops at "recorded", no Resume/DB).
	rec := postComplete(t, web, opc, token, `{"status":"complete","output":{"k":1}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200", rec.Code, rec.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "recorded" {
		t.Fatalf("status=%q want recorded", got["status"])
	}

	// Duplicate callback → still 200 recorded, no second effect.
	rec2 := postComplete(t, web, opc, token, `{"status":"complete","output":{"k":2}}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("dup code=%d want 200", rec2.Code)
	}
	_ = rcid // poll/status moved to txco://continuation-result (server pkg)
}
