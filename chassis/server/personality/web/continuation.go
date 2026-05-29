package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// writeJSON is the bus-bypassing reply for the continuation endpoints
// (they do not flow through the opstack / inlet render path).
func (web *WebController) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Callback acks are transient state, not cacheable. (POST responses
	// aren't cached by default; explicit no-store is belt-and-suspenders.)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		web.pu.Logger.Error("continuation write error", zap.String("err", err.Error()))
	}
}

// bearerToken extracts the single-use callback credential. The worker
// echoes it as `Authorization: Bearer <token>`; the outbound header name
// is also accepted for symmetry.
func bearerToken(r *http.Request) string {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(a[len("Bearer "):])
	}
	return r.Header.Get("X-Txco-Continuation-Token")
}

// handleContinuationComplete is POST /_txc/continuations/op/{opc}/complete.
// It records exactly one op result (create-if-absent ⇒ idempotent) and,
// when that completes the stage's barrier, the single ClaimResume winner
// drives the run forward. Resolution is O(1): the opaque opc IS the key.
func (web *WebController) handleContinuationComplete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	opc := mux.Vars(r)["opc"]
	runs := web.pu.Runs
	if runs == nil {
		web.writeJSON(w, http.StatusNotFound, map[string]string{"error": "continuations disabled"})
		return
	}

	lk, err := runs.ResolveOpContinuation(ctx, opc)
	if err == continuation.ErrNotFound {
		web.writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown op continuation"})
		return
	}
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "lookup failed"})
		return
	}

	tok := bearerToken(r)
	if tok == "" || subtle.ConstantTimeCompare([]byte(continuation.HashToken(tok)), []byte(lk.TokenHash)) != 1 {
		web.writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bad token"})
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in struct {
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(body, &in)

	termStatus := "completed"
	payload := []byte("{}")
	if in.Status == "failed" || in.Status == "fail" {
		termStatus = "failed"
		if len(in.Error) > 0 {
			payload = in.Error
		} else {
			payload = []byte(`{"error":{"message":"worker reported failure"}}`)
		}
	} else if len(in.Output) > 0 {
		payload = in.Output
	}

	// Deferred-join op: its terminal lives at the opc-keyed deferred location,
	// and the join stage is resolved dynamically (unknown at dispatch). Record
	// there, then resume only if the run has already suspended waiting on it.
	if lk.Deferred {
		web.handleDeferredComplete(w, ctx, opc, lk, termStatus, payload)
		return
	}

	recorded, err := runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, termStatus, payload)
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record failed"})
		return
	}
	if !recorded {
		// Duplicate / late callback: first terminal is authoritative.
		web.writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}

	ss, err := runs.ReadStageSuspended(ctx, lk.RunID, lk.Stage)
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stage read failed"})
		return
	}
	state, err := runs.StageState(ctx, lk.RunID, lk.Stage, ss.Manifest)
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state failed"})
		return
	}
	if state != continuation.StateResumable {
		web.writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}

	won, err := runs.ClaimResume(ctx, lk.RunID, lk.Stage)
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "claim failed"})
		return
	}
	if !won {
		web.writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}

	// Sole resumer. Drive the run forward under a trace request so the
	// resumed pipeline (merge → advance → render) appears in the trace
	// list — the worker callback itself is bus-bypassing/untraced, but
	// the resume it triggers is the real opstack work. Runs in the
	// controller ctx (the request ctx dies when this handler returns;
	// the original client already has its 202). NoopSink (trace off)
	// makes this free.
	tracer := web.sink.Begin(trace.RequestInfo{
		RID:       continuation.ResumeTraceRID(lk.RunID, lk.Stage),
		Src:       "continuation",
		Stack:     lk.Stage,
		StartedAt: time.Now(),
	})
	rctx := trace.WithContext(web.ctx, tracer)
	// Link this resume trace back to the run / originating request so
	// admin-ui can cross-navigate. ReadRunCreated is O(1).
	if rc, rcErr := runs.ReadRunCreated(rctx, lk.RunID); rcErr == nil {
		tracer.Event(trace.TimelineEvent{
			Ts:    time.Now(),
			Event: "continuation.resume",
			Fields: map[string]any{
				"run_id":              lk.RunID,
				"run_continuation_id": rc.RunContinuationID,
				"origin_rid":          rc.OriginRID,
				"stage":               lk.Stage,
				"stack_version_id":    rc.StackVersionID,
			},
		})
	}
	rerr := web.pu.Resume(rctx, lk.RunID, lk.Stage)
	status := "ok"
	var final []byte
	if rerr != nil {
		status = "error"
	} else if res, ok, _ := runs.ReadResult(rctx, lk.RunID); ok {
		final = res
	}
	tracer.End(status, final)
	if rerr != nil {
		web.pu.Logger.Error("resume failed",
			zap.String("run", lk.RunID), zap.String("stage", lk.Stage), zap.Error(rerr))
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "resume failed"})
		return
	}
	web.writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// handleDeferredComplete records a deferred-join op's terminal at its
// opc-keyed location, then drives resume IFF the run has already suspended at
// the (dynamically-resolved) join stage waiting on this op. If it hasn't, the
// run is still in-request and its in-request floor check reads the terminal
// directly — so we just record and ack. DriveDeferredResume re-checks
// resumability and ClaimResume-dedupes against the in-request race guard, so
// exactly one resume runs regardless of ordering.
func (web *WebController) handleDeferredComplete(w http.ResponseWriter, ctx context.Context, opc string, lk continuation.OpContinuationLookup, status string, payload []byte) {
	runs := web.pu.Runs
	recorded, err := runs.RecordDeferredTerminal(ctx, lk.RunID, opc, status, payload)
	if err != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record failed"})
		return
	}
	if !recorded {
		web.writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}
	stage, exists, serr := runs.ReadDeferredSuspendedAt(ctx, lk.RunID, opc)
	if serr != nil {
		web.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "suspend lookup failed"})
		return
	}
	if !exists {
		web.writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}
	web.pu.DriveDeferredResume(lk.RunID, stage)
	web.writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

// NOTE: the client poll / deferred-response endpoint
// (`GET /_txc/continuations/{rcid}`) was removed. Polling is now the
// same app URL + `?_txc.continuation=<rcid>`, handled through the normal
// traced pipeline by txco://continuation-result (see detectTenantBody
// short-circuit and the internal _sys `txc-continuation` stack). This
// preserves the deferred-response rendering (and `?format=json`) while
// making the poll appear in the trace list.
