package admin

// Live runs: the runs in flight in THIS chassis process, and the abort of
// one — the way to make a run (or every run of a stack) stop without
// stopping the chassis. Backed by the processor's live-run registry
// (chassis/processor/liveruns.go): the server registers each request
// goroutine's run; an abort cancels its context with who asked as the
// cause, and the scope loop's cancel branch ends it. A hard stop, no
// cleanup hook; the run's trace says `aborted` and by whom.
//
//	GET  /v1/tenants/{t}/runs                    run:*:read
//	POST /v1/tenants/{t}/runs/{rid}/abort        run:*:abort   {reason}
//	POST /v1/tenants/{t}/stacks/{name}/abort     run:*:abort   {reason}
//
// Scope is the tenant in the URL: a run of another tenant is not listed and
// answers 404 to an abort. On a fleet, each node holds its own registry: the
// list is the admin plane's own process's, and an abort is applied here AND
// published as a run.abort control event, which every node applies to its
// own registry (chassis/controlapply) — the one that holds the run ends it,
// a pump tick later. An abort by rid then answers 202 with `published:
// true` when the run was not in this process, rather than 404: whether a
// node has it is not known here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// abortReasonMax bounds the reason a caller may attach: it lands in the
// trace and the audit log, not in a database column.
const abortReasonMax = 200

type liveRunRecord struct {
	RID       string `json:"rid"`
	Tenant    string `json:"tenant"`
	Src       string `json:"src"`
	Entry     string `json:"entry,omitempty"`
	Stack     string `json:"stack,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Started   string `json:"started"`
	AgeMs     int64  `json:"age_ms"`
	AbortedBy string `json:"aborted_by,omitempty"`
}

type listRunsResponse struct {
	Runs []liveRunRecord `json:"runs"`
}

type abortRequest struct {
	Reason string `json:"reason"`
}

type abortResponse struct {
	Aborted int    `json:"aborted"`
	RID     string `json:"rid,omitempty"`
	Stack   string `json:"stack,omitempty"`
	// Published is set on a fleet: the abort was also sent to every node as
	// a control event, and Aborted counts only this process's.
	Published bool `json:"published,omitempty"`
}

// handleListRuns answers the tenant's runs in flight in this process.
func (c *Controller) handleListRuns(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "run:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_missing", nil)
		return
	}
	now := time.Now()
	runs := []liveRunRecord{}
	for _, lr := range c.pu.Live.List(ac.TenantSlug) {
		runs = append(runs, liveRunRecord{
			RID: lr.RID, Tenant: lr.Tenant, Src: lr.Src,
			Entry: lr.Entry, Stack: lr.Stack, Stage: lr.Stage,
			Started:   lr.Started.UTC().Format(time.RFC3339Nano),
			AgeMs:     now.Sub(lr.Started).Milliseconds(),
			AbortedBy: lr.AbortedBy,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, listRunsResponse{Runs: runs})
}

// handleAbortRun ends one run by rid. 404 when no run of the tenant has it
// — it finished already, or it is on another node.
func (c *Controller) handleAbortRun(w http.ResponseWriter, r *http.Request) {
	ac, by, reason, ok := c.abortCall(w, r)
	if !ok {
		return
	}
	rid := strings.TrimSpace(mux.Vars(r)["rid"])
	if rid == "" {
		writeJSONError(w, http.StatusBadRequest, "rid_missing", nil)
		return
	}
	n := 0
	if c.pu.Live.Abort(ac.TenantSlug, rid, by, reason) {
		n = 1
	}
	published, perr := c.publishRunAbort(r.Context(), ac, controlevent.RunAbortArtifact{Tenant: ac.TenantSlug, RID: rid, By: by, Reason: reason})
	if perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "run_abort_publish", map[string]any{"err": perr.Error(), "aborted_here": n})
		return
	}
	if n == 0 && !published {
		writeJSONError(w, http.StatusNotFound, "run_not_live",
			map[string]any{"rid": rid, "hint": "no run with this id is in flight on this chassis"})
		return
	}
	c.pu.Logger.Info("run aborted",
		zap.String("tenant_slug", ac.TenantSlug),
		zap.String("rid", rid),
		zap.String("by", by),
		zap.String("reason", reason),
		zap.Int("aborted_here", n),
		zap.Bool("published", published),
		zap.String("actor_id", ac.ActorID),
		zap.String("key_id", ac.KeyID),
		zap.String("source", ac.Source))
	status := http.StatusOK
	if n == 0 {
		status = http.StatusAccepted // not here; a node that has it will end it
	}
	writeJSON(w, status, abortResponse{Aborted: n, RID: rid, Published: published})
}

// handleAbortStack ends every live run of the tenant that entered at the
// stack or is in one of its scopes now. Zero is an answer, not an error:
// the stack had nothing in flight here.
func (c *Controller) handleAbortStack(w http.ResponseWriter, r *http.Request) {
	ac, by, reason, ok := c.abortCall(w, r)
	if !ok {
		return
	}
	stack := strings.TrimSpace(mux.Vars(r)["name"])
	if stack == "" {
		writeJSONError(w, http.StatusBadRequest, "stack_missing", nil)
		return
	}
	n := c.pu.Live.AbortStack(ac.TenantSlug, stack, by, reason)
	published, perr := c.publishRunAbort(r.Context(), ac, controlevent.RunAbortArtifact{Tenant: ac.TenantSlug, Stack: stack, By: by, Reason: reason})
	if perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "run_abort_publish", map[string]any{"err": perr.Error(), "aborted_here": n})
		return
	}
	c.pu.Logger.Info("stack runs aborted",
		zap.String("tenant_slug", ac.TenantSlug),
		zap.String("stack", stack),
		zap.Int("aborted_here", n),
		zap.Bool("published", published),
		zap.String("by", by),
		zap.String("reason", reason),
		zap.String("actor_id", ac.ActorID),
		zap.String("key_id", ac.KeyID),
		zap.String("source", ac.Source))
	writeJSON(w, http.StatusOK, abortResponse{Aborted: n, Stack: stack, Published: published})
}

// publishRunAbort sends the abort to the fleet as a run.abort control event
// — artifact first, then the outbox row, as every producer does — so the
// node that holds the run applies it. False, and no error, on a single
// chassis (no feed sink).
func (c *Controller) publishRunAbort(ctx context.Context, ac *auth.Context, art controlevent.RunAbortArtifact) (bool, error) {
	if !c.fleetEnabled() {
		return false, nil
	}
	art.At = time.Now().UTC().Format(time.RFC3339Nano)
	key := "runs/abort/" + hxid.NewTimeSort().String()
	ref, sum, _, err := c.fleetUploadArtifact(ctx, key, art)
	if err != nil {
		return false, fmt.Errorf("upload run.abort artifact: %w", err)
	}
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin run.abort tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := c.fleetQueueEvent(ctx, tx, controlevent.TypeRunAbort, ac.TenantID, "", 0, 0, ref, sum); err != nil {
		return false, fmt.Errorf("queue run.abort: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit run.abort: %w", err)
	}
	return true, nil
}

// abortCall is the shared head of the two abort handlers: the capability,
// the tenant, who is asking (the actor, or the operator's source when there
// is no actor row) and the optional reason from the body.
func (c *Controller) abortCall(w http.ResponseWriter, r *http.Request) (ac *auth.Context, by, reason string, ok bool) {
	if err := policy.RequireCapability(r.Context(), "run:*:abort"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return nil, "", "", false
	}
	ac = auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_missing", nil)
		return nil, "", "", false
	}
	by = ac.ActorID
	if by == "" {
		by = ac.Source
	}
	var req abortRequest
	if body, err := io.ReadAll(io.LimitReader(r.Body, 4096)); err == nil && len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
			return nil, "", "", false
		}
	}
	reason = strings.TrimSpace(req.Reason)
	if rs := []rune(reason); len(rs) > abortReasonMax {
		reason = string(rs[:abortReasonMax])
	}
	return ac, by, reason, true
}
