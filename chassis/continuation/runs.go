package continuation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/mr-tron/base58"
)

// Runs is the domain wrapper over a Store. Every method is expressed in
// terms of immutable docs + create-if-absent. There is no update path:
// run/stage status is DERIVED from which docs exist (see StageState /
// RunState).
type Runs struct{ s Store }

func NewRuns(s Store) *Runs { return &Runs{s: s} }

// ---- identities -----------------------------------------------------------

// newRunID is the internal, time-sortable execution id (sorts the runs/
// listing). Not client- or worker-facing.
func newRunID() string { return "run_" + hxid.New().String() }

// randID returns prefix + base58(16 crypto-random bytes): ~128 bits,
// unguessable. Used for the client- and worker-facing handles, which are
// the most exposed surface.
func randID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + base58.Encode(b[:]), nil
}

// MintToken returns a single-use bearer (256-bit) and its sha256 hex.
// The token is handed to the worker out of band of the body; only the
// hash is stored. Machine-to-machine, so high-entropy random (not the
// human-transcribable word secret) is the right primitive.
func MintToken() (token, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	return token, HashToken(token), nil
}

// NewOpContinuationID mints a worker-facing per-async-op handle.
func NewOpContinuationID() (string, error) { return randID("opc_") }

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ---- doc types ------------------------------------------------------------

type RunCreated struct {
	RunID             string `json:"run_id"`
	RunContinuationID string `json:"run_continuation_id"`
	TenantID          string `json:"tenant_id,omitempty"`
	Stack             string `json:"stack"`
	StackVersionID    string `json:"stack_version_id,omitempty"`
	FirstStage        string `json:"first_stage"`
	// OriginRID is the trace rid of the request that suspended into this
	// run — the back-pointer that lets admin-ui link the resume trace to
	// the originating request (and vice-versa).
	OriginRID string    `json:"origin_rid,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// requestContinuationLookup maps an originating request rid → its run.
// Mirror of runContinuationLookup (rcid → run); written create-if-absent
// at suspend so the trace detail handler can resolve rid → run in O(1).
type requestContinuationLookup struct {
	RunID             string `json:"run_id"`
	RunContinuationID string `json:"run_continuation_id"`
}

type runContinuationLookup struct {
	RunID string `json:"run_id"`
}

type OpContinuationLookup struct {
	RunID     string    `json:"run_id"`
	Stage     string    `json:"stage"`
	Op        string    `json:"op"`
	Ordinal   int       `json:"ordinal"`
	TokenHash string    `json:"token_hash"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Deferred marks an op-continuation whose terminal is filed under an
	// opc-keyed location (deferred join), not a fixed stage. The callback
	// handler routes a Deferred lookup to RecordDeferredTerminal and never
	// uses Stage (which is empty for deferred ops — the join scope is
	// resolved dynamically at run time). See chassis/continuation/deferred.go.
	Deferred bool `json:"deferred,omitempty"`
}

type OpManifestEntry struct {
	Ordinal int    `json:"ordinal"`
	Op      string `json:"op"`
	Async   bool   `json:"async"`
	// OpContinuationID, when set, marks a manifest entry as a deferred join:
	// its terminal lives under the opc-keyed deferred location rather than
	// this stage's (ordinal, op) key. Resume reads it via ReadDeferredTerminal.
	// Empty for ordinary same-scope ops (the existing path).
	OpContinuationID string `json:"op_continuation_id,omitempty"`
}

type StageSuspended struct {
	Stage         string            `json:"stage"`
	ScopeEnvelope string            `json:"scope_envelope"` // raw JSON entering the stage
	Manifest      []OpManifestEntry `json:"manifest"`
	StackVersion  string            `json:"stack_version_id,omitempty"`
	SuspendedAt   time.Time         `json:"suspended_at"`
}

type opCreated struct {
	Ordinal          int       `json:"ordinal"`
	Op               string    `json:"op"`
	Async            bool      `json:"async"`
	OpContinuationID string    `json:"op_continuation_id,omitempty"`
	InputKey         string    `json:"input_key"`
	CreatedAt        time.Time `json:"created_at"`
}

type OpTerminal struct {
	Status     string    `json:"status"` // "completed" | "failed"
	OutputKey  string    `json:"output_key,omitempty"`
	ErrorKey   string    `json:"error_key,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// ---- key builders (logical, "/"-separated; store sanitizes segments) ------

func runContinuationKey(rcid string) string { return "run-continuations/" + rcid + ".json" }
func opContinuationKey(opc string) string   { return "op-continuations/" + opc + ".json" }
func runDir(runID string) string            { return "runs/" + runID }
func runCreatedKey(runID string) string     { return runDir(runID) + "/run-created.json" }
func requestContinuationKey(rid string) string {
	return "request-continuations/" + rid + ".json"
}
func opstackSnapshotKey(runID string) string {
	return runDir(runID) + "/opstack-snapshot.json"
}

// ResumeTraceRID is the trace RID for resuming `stage` of `runID`. It
// embeds the stage so a multi-stage run produces one distinct, linkable
// trace per resumed stage (not a single colliding `resume-<runID>`).
// runID is "run_"+base58 (no '-'), so the first '-' after it
// unambiguously separates runID from the sanitized stage — see
// ParseResumeTraceRID. The stage is sanitized to [A-Za-z0-9_-] so the
// whole RID stays within the admin trace-id allowlist (validRID) and is
// a safe trace dir name.
func ResumeTraceRID(runID, stage string) string {
	return "resume-" + runID + "-" + sanitizeStageForRID(stage)
}

// ParseResumeTraceRID extracts runID from a ResumeTraceRID. ok=false for
// any rid not of that shape (e.g. a normal request rid).
func ParseResumeTraceRID(rid string) (runID string, ok bool) {
	rest, found := strings.CutPrefix(rid, "resume-")
	if !found {
		return "", false
	}
	i := strings.IndexByte(rest, '-')
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

func sanitizeStageForRID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}

// ResumeRef is one resumed stage of a run and its trace RID.
type ResumeRef struct {
	RID   string `json:"rid"`
	Stage string `json:"stage"`
}

// TraceLinks is the cross-navigation data for a continuation's traces:
// the originating request rid and one resume trace per suspended stage.
type TraceLinks struct {
	RunID             string      `json:"run_id"`
	RunContinuationID string      `json:"run_continuation_id,omitempty"`
	OriginRID         string      `json:"origin_rid,omitempty"`
	Resumes           []ResumeRef `json:"resumes,omitempty"`
}

// ReadTraceLinks assembles the trace linkage for a run: rcid + origin
// rid (from run-created) and a stable, deterministic resume RID per
// suspended stage (from the stage-suspended docs). Used by the admin
// trace-detail handler to render inline cross-links.
func (r *Runs) ReadTraceLinks(ctx context.Context, runID string) (TraceLinks, error) {
	rc, err := r.ReadRunCreated(ctx, runID)
	if err != nil {
		return TraceLinks{}, err
	}
	tl := TraceLinks{
		RunID:             runID,
		RunContinuationID: rc.RunContinuationID,
		OriginRID:         rc.OriginRID,
	}
	keys, err := r.s.List(ctx, runDir(runID)+"/stages")
	if err != nil {
		return tl, nil // run exists but no stages yet — links are just origin
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, "/stage-suspended.json") {
			continue
		}
		b, _, gerr := r.s.Get(ctx, k)
		if gerr != nil {
			continue
		}
		var d StageSuspended
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		tl.Resumes = append(tl.Resumes, ResumeRef{
			RID:   ResumeTraceRID(runID, d.Stage),
			Stage: d.Stage,
		})
	}
	sort.SliceStable(tl.Resumes, func(i, j int) bool {
		return tl.Resumes[i].Stage < tl.Resumes[j].Stage
	})
	return tl, nil
}
func resultKey(runID string) string       { return runDir(runID) + "/result.json" }
func runFailedKey(runID string) string    { return runDir(runID) + "/failed.json" }
func stageDir(runID, stage string) string { return runDir(runID) + "/stages/" + stage }
func stageSuspendedKey(runID, stage string) string {
	return stageDir(runID, stage) + "/stage-suspended.json"
}
func stageFailedKey(runID, stage string) string {
	return stageDir(runID, stage) + "/stage-failed.json"
}
func resumeClaimKey(runID, stage string) string {
	return stageDir(runID, stage) + "/resume-claim.json"
}
func opDir(runID, stage string, ordinal int, op string) string {
	return fmt.Sprintf("%s/ops/%04d-%s", stageDir(runID, stage), ordinal, op)
}
func opCreatedKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/op-created.json"
}
func opAcceptedKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/op-accepted.json"
}
func opTerminalKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/op-terminal.json"
}
func opInputKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/input.json"
}
func opOutputKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/output.json"
}
func opErrorKey(runID, stage string, ordinal int, op string) string {
	return opDir(runID, stage, ordinal, op) + "/error.json"
}

// ---- helpers --------------------------------------------------------------

// createJSON marshals v and Creates it. ErrExists is returned to the
// caller verbatim — callers decide whether a duplicate is a no-op
// (idempotent re-suspend) or a lost race (claims).
func (r *Runs) createJSON(ctx context.Context, key string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = r.s.Create(ctx, key, bytes.NewReader(b), Meta{ContentType: "application/json"})
	return err
}

func ignoreExists(err error) error {
	if err == ErrExists {
		return nil
	}
	return err
}

// ---- run / stage / op lifecycle ------------------------------------------

// CreateRun mints internal + client identities and writes the immutable
// run-created doc plus the run-continuation lookup. Fresh ids ⇒ no
// collision; ErrExists would be a genuine fault, so it is not swallowed.
func (r *Runs) CreateRun(ctx context.Context, tenantID, stack, stackVersionID, firstStage, originRID string, expires time.Time) (runID, rcid string, err error) {
	runID = newRunID()
	rcid, err = randID("rc_")
	if err != nil {
		return "", "", err
	}
	rc := RunCreated{
		RunID: runID, RunContinuationID: rcid, TenantID: tenantID,
		Stack: stack, StackVersionID: stackVersionID, FirstStage: firstStage,
		OriginRID: originRID,
		CreatedAt: time.Now().UTC(), ExpiresAt: expires,
	}
	if err = r.createJSON(ctx, runCreatedKey(runID), rc); err != nil {
		return "", "", err
	}
	if err = r.createJSON(ctx, runContinuationKey(rcid), runContinuationLookup{RunID: runID}); err != nil {
		return "", "", err
	}
	// Reverse lookup so admin-ui can navigate originating-request →
	// run → resume traces. Best-effort: a duplicate rid (same request
	// somehow suspending twice) is tolerated rather than failing the run.
	if originRID != "" {
		if e := r.createJSON(ctx, requestContinuationKey(originRID),
			requestContinuationLookup{RunID: runID, RunContinuationID: rcid}); ignoreExists(e) != nil {
			return "", "", e
		}
	}
	return runID, rcid, nil
}

// SuspendStage writes the per-stage immutable manifest + scope-entry
// envelope. Idempotent: a re-entered suspend for the same stage is a
// no-op (ErrExists swallowed).
func (r *Runs) SuspendStage(ctx context.Context, runID, stage, scopeEnvelope, stackVersionID string, manifest []OpManifestEntry) error {
	doc := StageSuspended{
		Stage: stage, ScopeEnvelope: scopeEnvelope, Manifest: manifest,
		StackVersion: stackVersionID, SuspendedAt: time.Now().UTC(),
	}
	return ignoreExists(r.createJSON(ctx, stageSuspendedKey(runID, stage), doc))
}

// OpRecordSpec is one op's durable record, written before any dispatch.
type OpRecordSpec struct {
	Ordinal          int
	Op               string
	Async            bool
	OpContinuationID string
	TokenHash        string
	Input            []byte
	ExpiresAt        time.Time
}

// CreateOpRecords writes, for every op: input blob + op-created doc; and
// for each async op the op-continuation lookup. All create-if-absent and
// idempotent (ErrExists swallowed) so a re-entered suspend is safe.
func (r *Runs) CreateOpRecords(ctx context.Context, runID, stage string, specs []OpRecordSpec) error {
	for _, sp := range specs {
		ik := opInputKey(runID, stage, sp.Ordinal, sp.Op)
		if _, err := r.s.Create(ctx, ik, bytes.NewReader(sp.Input), Meta{ContentType: "application/json"}); ignoreExists(err) != nil {
			return err
		}
		oc := opCreated{
			Ordinal: sp.Ordinal, Op: sp.Op, Async: sp.Async,
			OpContinuationID: sp.OpContinuationID, InputKey: ik,
			CreatedAt: time.Now().UTC(),
		}
		if err := ignoreExists(r.createJSON(ctx, opCreatedKey(runID, stage, sp.Ordinal, sp.Op), oc)); err != nil {
			return err
		}
		if sp.Async {
			lk := OpContinuationLookup{
				RunID: runID, Stage: stage, Op: sp.Op, Ordinal: sp.Ordinal,
				TokenHash: sp.TokenHash, ExpiresAt: sp.ExpiresAt,
			}
			if err := ignoreExists(r.createJSON(ctx, opContinuationKey(sp.OpContinuationID), lk)); err != nil {
				return err
			}
		}
	}
	return nil
}

// RecordAccepted records a worker's 202 ack (informational; not required
// for terminal derivation).
func (r *Runs) RecordAccepted(ctx context.Context, runID, stage string, ordinal int, op, workerJobID string) error {
	return ignoreExists(r.createJSON(ctx, opAcceptedKey(runID, stage, ordinal, op), map[string]any{
		"worker_job_id": workerJobID,
		"accepted_at":   time.Now().UTC(),
	}))
}

// RecordTerminal writes the op's result blob then the create-if-absent
// op-terminal doc. The first terminal (success OR failure) wins; a
// duplicate/late callback gets ErrExists on op-terminal ⇒ recorded=false
// and is a harmless no-op. status must be "completed" or "failed".
func (r *Runs) RecordTerminal(ctx context.Context, runID, stage string, ordinal int, op, status string, payload []byte) (recorded bool, err error) {
	term := OpTerminal{Status: status, RecordedAt: time.Now().UTC()}
	switch status {
	case "completed":
		k := opOutputKey(runID, stage, ordinal, op)
		if _, e := r.s.Create(ctx, k, bytes.NewReader(payload), Meta{ContentType: "application/json"}); ignoreExists(e) != nil {
			return false, e
		}
		term.OutputKey = k
	case "failed":
		k := opErrorKey(runID, stage, ordinal, op)
		if _, e := r.s.Create(ctx, k, bytes.NewReader(payload), Meta{ContentType: "application/json"}); ignoreExists(e) != nil {
			return false, e
		}
		term.ErrorKey = k
	default:
		return false, fmt.Errorf("continuation: bad terminal status %q", status)
	}
	err = r.createJSON(ctx, opTerminalKey(runID, stage, ordinal, op), term)
	if err == ErrExists {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ReadStageSuspended loads the per-stage manifest + scope-entry envelope.
func (r *Runs) ReadStageSuspended(ctx context.Context, runID, stage string) (StageSuspended, error) {
	b, _, err := r.s.Get(ctx, stageSuspendedKey(runID, stage))
	if err != nil {
		return StageSuspended{}, err
	}
	var d StageSuspended
	return d, json.Unmarshal(b, &d)
}

// ReadOpTerminal loads an op's terminal doc (status + blob keys).
func (r *Runs) ReadOpTerminal(ctx context.Context, runID, stage string, ordinal int, op string) (OpTerminal, error) {
	b, _, err := r.s.Get(ctx, opTerminalKey(runID, stage, ordinal, op))
	if err != nil {
		return OpTerminal{}, err
	}
	var d OpTerminal
	return d, json.Unmarshal(b, &d)
}

func (r *Runs) Get(ctx context.Context, key string) ([]byte, error) {
	b, _, err := r.s.Get(ctx, key)
	return b, err
}

// WriteOpstackSnapshot stores the resolved opstack the run was suspended
// against. Create-if-absent: only the first suspend of a run writes it;
// later re-suspends of the same multi-stage run reuse it (immutable, so a
// later txco apply cannot change what this run resumes against).
func (r *Runs) WriteOpstackSnapshot(ctx context.Context, runID string, snapshot []byte) error {
	_, err := r.s.Create(ctx, opstackSnapshotKey(runID), bytes.NewReader(snapshot), Meta{ContentType: "application/json"})
	return ignoreExists(err)
}

// ReadOpstackSnapshot returns the run's frozen opstack snapshot. ErrNotFound
// (passed through) means no snapshot — the caller falls back to the live
// opstack (back-compat for pre-snapshot runs / unversioned _sys).
func (r *Runs) ReadOpstackSnapshot(ctx context.Context, runID string) ([]byte, error) {
	b, _, err := r.s.Get(ctx, opstackSnapshotKey(runID))
	return b, err
}

// ---- sweeper support ------------------------------------------------------

// ListRunIDs enumerates every run by walking the runs/ prefix and taking
// the distinct runID segment. Object-store walk, no index — the dumb
// chassis keeps run truth in the store, not a DB table.
func (r *Runs) ListRunIDs(ctx context.Context) ([]string, error) {
	keys, err := r.s.List(ctx, "runs")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) < 2 || parts[0] != "runs" || parts[1] == "" {
			continue
		}
		id := parts[1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// ReadResumeClaim reports when a stage's resume was claimed. exists=false
// (nil error) when no resumer has claimed it yet. A claim that has sat far
// longer than any legitimate resume means the resumer crashed mid-resume.
func (r *Runs) ReadResumeClaim(ctx context.Context, runID, stage string) (claimedAt time.Time, exists bool, err error) {
	b, _, gerr := r.s.Get(ctx, resumeClaimKey(runID, stage))
	if gerr == ErrNotFound {
		return time.Time{}, false, nil
	}
	if gerr != nil {
		return time.Time{}, false, gerr
	}
	var d struct {
		ClaimedAt time.Time `json:"claimed_at"`
	}
	if jerr := json.Unmarshal(b, &d); jerr != nil {
		return time.Time{}, false, jerr
	}
	return d.ClaimedAt, true, nil
}

// PurgeRun deletes every doc of a dead (terminal + past-retention) run.
// Lookup docs go FIRST so a concurrent resolve can never land on a
// half-deleted run; then the run dir. Delete is idempotent, so a partial
// previous purge is safely completed on the next pass. This is the only
// place continuation docs are deleted — it destroys a finished run, it
// never mutates a live one.
func (r *Runs) PurgeRun(ctx context.Context, runID string) error {
	rc, rcErr := r.ReadRunCreated(ctx, runID)

	runKeys, err := r.s.List(ctx, runDir(runID))
	if err != nil {
		return err
	}

	// Collect async op-continuation handles from the run's op-created docs
	// before we delete the run dir.
	var opcs []string
	for _, k := range runKeys {
		if !strings.HasSuffix(k, "/op-created.json") {
			continue
		}
		b, _, gerr := r.s.Get(ctx, k)
		if gerr != nil {
			continue
		}
		var oc opCreated
		if json.Unmarshal(b, &oc) == nil && oc.OpContinuationID != "" {
			opcs = append(opcs, oc.OpContinuationID)
		}
	}

	// Lookups first.
	for _, opc := range opcs {
		if e := r.s.Delete(ctx, opContinuationKey(opc)); e != nil {
			return e
		}
	}
	if rcErr == nil {
		if rc.RunContinuationID != "" {
			if e := r.s.Delete(ctx, runContinuationKey(rc.RunContinuationID)); e != nil {
				return e
			}
		}
		if rc.OriginRID != "" {
			if e := r.s.Delete(ctx, requestContinuationKey(rc.OriginRID)); e != nil {
				return e
			}
		}
	}

	// Then the run dir.
	for _, k := range runKeys {
		if e := r.s.Delete(ctx, k); e != nil {
			return e
		}
	}
	return nil
}

// ReadResult returns the final envelope and true if the run completed.
func (r *Runs) ReadResult(ctx context.Context, runID string) ([]byte, bool, error) {
	b, _, err := r.s.Get(ctx, resultKey(runID))
	if err == ErrNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ClaimResume create-if-absents the SINGLE deterministic resume-claim key
// for (run, stage). Exactly one caller wins (won=true); losers won=false.
func (r *Runs) ClaimResume(ctx context.Context, runID, stage string) (won bool, err error) {
	e := r.createJSON(ctx, resumeClaimKey(runID, stage), map[string]any{"claimed_at": time.Now().UTC()})
	if e == ErrExists {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return true, nil
}

// WriteResult write-once stores the final success envelope.
func (r *Runs) WriteResult(ctx context.Context, runID string, finalEnvelope []byte) error {
	_, err := r.s.Create(ctx, resultKey(runID), bytes.NewReader(finalEnvelope), Meta{ContentType: "application/json"})
	return ignoreExists(err)
}

// FailStage records a stage that cannot complete/advance (failed sibling
// op, or stack-version mismatch — no specific op failed). Not an
// op-terminal.
func (r *Runs) FailStage(ctx context.Context, runID, stage, reason string) error {
	return ignoreExists(r.createJSON(ctx, stageFailedKey(runID, stage), map[string]any{
		"reason": reason, "failed_at": time.Now().UTC(),
	}))
}

// FailRun records a run-level terminal failure (expiry/GC; v1 mostly
// future).
func (r *Runs) FailRun(ctx context.Context, runID, reason string) error {
	return ignoreExists(r.createJSON(ctx, runFailedKey(runID), map[string]any{
		"reason": reason, "failed_at": time.Now().UTC(),
	}))
}

func (r *Runs) ResolveRunContinuation(ctx context.Context, rcid string) (string, error) {
	b, _, err := r.s.Get(ctx, runContinuationKey(rcid))
	if err != nil {
		return "", err
	}
	var d runContinuationLookup
	if err := json.Unmarshal(b, &d); err != nil {
		return "", err
	}
	return d.RunID, nil
}

// ReadRunCreated returns the immutable run-created doc (rcid, stack,
// origin rid, …) for a runID.
func (r *Runs) ReadRunCreated(ctx context.Context, runID string) (RunCreated, error) {
	b, _, err := r.s.Get(ctx, runCreatedKey(runID))
	if err != nil {
		return RunCreated{}, err
	}
	var d RunCreated
	return d, json.Unmarshal(b, &d)
}

// ResolveRequestContinuation maps an originating request rid → runID
// (ErrNotFound when the request did not suspend into a continuation).
func (r *Runs) ResolveRequestContinuation(ctx context.Context, rid string) (string, error) {
	b, _, err := r.s.Get(ctx, requestContinuationKey(rid))
	if err != nil {
		return "", err
	}
	var d requestContinuationLookup
	if err := json.Unmarshal(b, &d); err != nil {
		return "", err
	}
	return d.RunID, nil
}

func (r *Runs) ResolveOpContinuation(ctx context.Context, opc string) (OpContinuationLookup, error) {
	b, _, err := r.s.Get(ctx, opContinuationKey(opc))
	if err != nil {
		return OpContinuationLookup{}, err
	}
	var d OpContinuationLookup
	return d, json.Unmarshal(b, &d)
}

// AppendEvent writes a distinct append-only audit object. Key carries a
// timestamp + random suffix so concurrent events never collide.
func (r *Runs) AppendEvent(ctx context.Context, runID, kind string, fields map[string]any) error {
	suf, err := randID("")
	if err != nil {
		return err
	}
	ts := time.Now().UTC()
	key := fmt.Sprintf("%s/events/%s-%s-%s.json", runDir(runID),
		ts.Format("20060102T150405.000000000Z"), suf, kind)
	doc := map[string]any{"ts": ts, "kind": kind}
	for k, v := range fields {
		doc[k] = v
	}
	return ignoreExists(r.createJSON(ctx, key, doc))
}

// ---- derived state --------------------------------------------------------

// State values.
const (
	StateWaiting   = "waiting"
	StateResumable = "resumable"
	StateCompleted = "completed"
	StateFailed    = "failed"
)

// StageState derives the state of one stage from doc existence, in the
// fixed precedence order. manifest is that stage's op manifest.
func (r *Runs) StageState(ctx context.Context, runID, stage string, manifest []OpManifestEntry) (string, error) {
	if ok, err := r.s.Exists(ctx, resultKey(runID)); err != nil {
		return "", err
	} else if ok {
		return StateCompleted, nil
	}
	if ok, err := r.s.Exists(ctx, runFailedKey(runID)); err != nil {
		return "", err
	} else if ok {
		return StateFailed, nil
	}
	if ok, err := r.s.Exists(ctx, stageFailedKey(runID, stage)); err != nil {
		return "", err
	} else if ok {
		return StateFailed, nil
	}
	for _, m := range manifest {
		// Deferred-join entries (OpContinuationID set) file their terminal
		// under the opc-keyed deferred location, not this stage's (ordinal,
		// op) key — the dispatch scope ≠ the join scope. Same precedence
		// otherwise. See chassis/continuation/deferred.go.
		key := opTerminalKey(runID, stage, m.Ordinal, m.Op)
		if m.OpContinuationID != "" {
			key = deferredTerminalKey(runID, m.OpContinuationID)
		}
		ok, err := r.s.Exists(ctx, key)
		if err != nil {
			return "", err
		}
		if !ok {
			return StateWaiting, nil
		}
	}
	return StateResumable, nil
}

// CurrentStage returns the latest suspended stage (max SuspendedAt) and
// its loaded doc. Used by the client GET to derive run state without any
// stored status.
func (r *Runs) CurrentStage(ctx context.Context, runID string) (StageSuspended, bool, error) {
	keys, err := r.s.List(ctx, runDir(runID)+"/stages")
	if err != nil {
		return StageSuspended{}, false, err
	}
	var stages []string
	for _, k := range keys {
		if strings.HasSuffix(k, "/stage-suspended.json") {
			stages = append(stages, k)
		}
	}
	if len(stages) == 0 {
		return StageSuspended{}, false, nil
	}
	var latest StageSuspended
	found := false
	for _, k := range stages {
		b, _, gerr := r.s.Get(ctx, k)
		if gerr != nil {
			return StageSuspended{}, false, gerr
		}
		var d StageSuspended
		if jerr := json.Unmarshal(b, &d); jerr != nil {
			return StageSuspended{}, false, jerr
		}
		if !found || d.SuspendedAt.After(latest.SuspendedAt) {
			latest, found = d, true
		}
	}
	// Defensive: stable order if timestamps tie.
	sort.SliceStable(stages, func(i, j int) bool { return stages[i] < stages[j] })
	return latest, found, nil
}

// RunState derives the overall run state for the client GET.
func (r *Runs) RunState(ctx context.Context, runID string) (string, error) {
	// Run-level terminal docs win regardless of stage docs — precedence
	// 1 & 2 of the derived-state order. CurrentStage/StageState only
	// apply these when a stage-suspended doc exists; a completed/failed
	// run must report so even if (e.g.) the result was written without a
	// per-stage doc.
	if ok, e := r.s.Exists(ctx, resultKey(runID)); e != nil {
		return "", e
	} else if ok {
		return StateCompleted, nil
	}
	if ok, e := r.s.Exists(ctx, runFailedKey(runID)); e != nil {
		return "", e
	} else if ok {
		return StateFailed, nil
	}

	cur, ok, err := r.CurrentStage(ctx, runID)
	if err != nil {
		return "", err
	}
	if !ok {
		// Run created but no stage suspended yet, or unknown.
		if okc, e := r.s.Exists(ctx, runCreatedKey(runID)); e != nil {
			return "", e
		} else if okc {
			return StateWaiting, nil
		}
		return "", ErrNotFound
	}
	return r.StageState(ctx, runID, cur.Stage, cur.Manifest)
}
