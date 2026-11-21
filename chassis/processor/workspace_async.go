package processor

// `workspace://<name>/exec WITH mode = "async", max_duration = …`
//
// The third kind of async op. A remote-async op posts a job to a worker
// that calls back; a local-async op runs in a goroutine here. A workspace
// async op hands its command to a MACHINE that reports the command's end
// itself — an attached machine, which may be disconnected for most of the
// command's life — and nothing anywhere is held open while it runs.
//
//	dispatch (the node that suspended the run)
//	  records where the result belongs (RecordDispatch), then hands the
//	  command over with the op's continuation and token. The machine starts
//	  it detached and answers at once.
//	completion (whichever node holds the machine's link when it ends)
//	  CompleteWorkspaceAsync checks the token and the dispatch record,
//	  builds the op's result, records the terminal and claims the resume.
//
// The result is built HERE, by the chassis, from typed fields: two output
// strings, an exit code and a few flags, placed under the `into` the
// dispatch recorded. A machine never supplies envelope JSON, so it can no
// more write outside `into` or forge `_txc.workspace.*` than a command's
// output can on the ordinary path — which is why the terminal carries the
// trusted "workspace" transport, exactly as a held exec's does.
//
// A command that fails, cannot be delivered (`away`) or runs out of time is
// a COMPLETED terminal with `workspace.error` inside, never a failed one: a
// failed terminal ends the stage, and the scopes that read the outcome
// would never run.

import (
	"context"
	"crypto/subtle"
	"errors"
	"regexp"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// workspaceAsyncHandoff bounds the handoff of a command to its machine when
// the op sets no WITH timeout. The command's own life is max_duration.
const workspaceAsyncHandoff = 30 * time.Second

// workspaceAsyncMargin is how much sooner than the run's expiry the
// machine ends the command, so a connected machine's "timeout" arrives
// before the run is swept.
const workspaceAsyncMargin = time.Minute

// workspaceDispatch is the chassis's own record of an async workspace op's
// dispatch (continuation.Runs.RecordDispatch).
type workspaceDispatch struct {
	// Into is where the result goes in the envelope, as the op resolved it.
	Into string `json:"into"`
	// Tenant and WorkspaceID say whose command it is. A result is accepted
	// only from a machine holding that workspace.
	Tenant       string    `json:"tenant"`
	WorkspaceID  string    `json:"workspace_id"`
	DispatchedAt time.Time `json:"dispatched_at"`
}

// workspaceAsync rides the context of one async workspace op's dispatch,
// from dispatchWorkspaceAsync into ExecWorkspace and back.
type workspaceAsync struct {
	opc, token string
	life       time.Duration
	record     func(workspaceDispatch) error

	// Set by ExecWorkspace. started: the machine took the command, as job.
	// pending: the handoff's outcome is unknown; it may have.
	started bool
	pending bool
	job     string
}

type ctxKeyWorkspaceAsync struct{}

func withWorkspaceAsync(ctx context.Context, a *workspaceAsync) context.Context {
	return context.WithValue(ctx, ctxKeyWorkspaceAsync{}, a)
}

func workspaceAsyncFrom(ctx context.Context) *workspaceAsync {
	a, _ := ctx.Value(ctxKeyWorkspaceAsync{}).(*workspaceAsync)
	return a
}

// workspaceAsyncLife reads the command's life from WITH max_duration. It is
// required: without it the run has no expiry, and a machine that never
// returns would leave it waiting forever.
func workspaceAsyncLife(op operation.Operation) (time.Duration, error) {
	v := gjson.Get(op.Meta, "max_duration")
	if !v.Exists() {
		return 0, errors.New(`mode = "async" on a workspace needs WITH max_duration: the command's life, and the run's expiry`)
	}
	d, err := time.ParseDuration(v.String())
	if err != nil || d <= 0 {
		return 0, errors.New(`WITH max_duration must be a duration such as "1h"`)
	}
	// The machine gives up a little before the run does.
	margin := workspaceAsyncMargin
	if d < 10*margin {
		margin = d / 10
	}
	return d - margin, nil
}

// dispatchWorkspaceAsync hands one async workspace op to its machine. It
// reports whether the op's terminal was recorded here (settled): no result
// will arrive later for such an op, so the caller drives the resume.
func (pu *Unit) dispatchWorkspaceAsync(ctx context.Context, op operation.Operation, runID, stage string, ordinal int, name, opc, token string) (settled bool) {
	into := boundedInto(gjson.Get(op.Meta, "into").String())
	if into == "" {
		into = workspaceDefaultInto
	}
	aStart := time.Now()
	step := func(status, errMsg string, out []byte) {
		trace.FromContext(ctx).Step(trace.StepInfo{
			Stack: op.Stack, Scope: op.Scope, Name: name,
			Operation: op.Resonator.Exec, Transport: "async",
			Input:     []byte(op.Input),
			Output:    out,
			StartedAt: aStart, FinishedAt: time.Now(),
			Status: status, Error: errMsg, StackVersion: pu.tracedStackVersion(ctx, op.Stack),
		})
	}
	// settle records the terminal now. Always "completed": see the note at
	// the top of this file.
	settle := func(pl event.Payload) bool {
		if _, err := pu.Runs.RecordTerminal(ctx, runID, stage, ordinal, name, "completed",
			continuation.TerminalMeta{Transport: "workspace"}, []byte(pl.Raw)); err != nil {
			pu.Logger.Error("workspace async: RecordTerminal failed",
				zap.String("run", runID), zap.String("stage", stage), zap.String("op", name), zap.Error(err))
		}
		step("ok", "", []byte(pl.Raw))
		return true
	}

	life, err := workspaceAsyncLife(op)
	if err != nil {
		return settle(workspaceFailure(into, "", "", "bad_request", err.Error(), 0))
	}
	handoff := workspaceAsyncHandoff
	if gjson.Get(op.Meta, "timeout").Exists() {
		t, over := pu.opMetaTimeout(op)
		if over {
			return settle(workspaceFailure(into, "", "", "bad_request",
				`WITH timeout bounds only the handoff of an async exec and exceeds op-timeout-max; the command's life is max_duration`, 0))
		}
		handoff = t
	}
	ad := &workspaceAsync{opc: opc, token: token, life: life,
		record: func(d workspaceDispatch) error {
			return pu.Runs.RecordDispatch(ctx, runID, stage, ordinal, name, d)
		}}
	octx, cancel := context.WithTimeout(withWorkspaceAsync(ctx, ad), handoff)
	defer cancel()

	out, _, eerr := pu.Exec(octx, op)
	switch {
	case eerr != nil:
		// An authoring error (a malformed ref, a bad name). The stage cannot
		// go on; this is the one failed terminal.
		_, _ = pu.Runs.RecordTerminal(ctx, runID, stage, ordinal, name, "failed",
			continuation.TerminalMeta{Transport: "workspace"}, failPayload(eerr.Error()))
		step("error", eerr.Error(), nil)
		return true
	case ad.started:
		_ = pu.Runs.RecordAccepted(ctx, runID, stage, ordinal, name, ad.job)
		ack, _ := sjson.Set(`{"status":"accepted"}`, "job_id", ad.job)
		step("pending", "", []byte(ack))
		return false
	case ad.pending:
		step("pending", "", []byte(`{"status":"unknown","note":"the handoff was not confirmed; the machine's own report or the run's expiry settles it"}`))
		return false
	}
	payload := out.Raw
	if out.Type == event.Null || payload == "" {
		payload = "{}"
	}
	return settle(event.Payload{Raw: payload, Type: event.JSON})
}

var errCodeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// CompleteWorkspaceAsync records the end of a command a workspace async op
// started, as its machine reported it, and resumes the run if that was the
// last thing its stage was waiting for.
//
// It is called on the node that holds the machine's link, with what that
// node knows for itself: the tenant and workspace the machine's credential
// is for, and the computer's name. The machine supplies the continuation,
// its token and the result.
//
// The result is accepted only if ALL of these hold: the continuation
// exists; the token is its token; and the chassis's own dispatch record
// says this op was sent to this workspace of this tenant.
//
// A nil return means the result is durably recorded (now, or already) and
// the resume is claimed or not yet due: the machine may delete its copy. A
// *workspace.Error is a refusal that will never change, so the machine
// drops the result. Any other error is passing trouble: the machine keeps
// its copy and sends again.
func (pu *Unit) CompleteWorkspaceAsync(ctx context.Context, tenant, workspaceID, computer, opc, token string, res workspace.AsyncResult) error {
	if pu.Runs == nil {
		return &workspace.Error{Code: "unsupported", Message: "continuations are disabled on this chassis"}
	}
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if errors.Is(err, continuation.ErrNotFound) {
		return &workspace.Error{Code: workspace.CodeNotFound, Message: "unknown continuation: the run is gone"}
	}
	if err != nil {
		return err
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(continuation.HashToken(token)), []byte(lk.TokenHash)) != 1 {
		return &workspace.Error{Code: "not_allowed", Message: "bad token"}
	}
	if lk.Deferred {
		return &workspace.Error{Code: "not_allowed", Message: "not a workspace op's continuation"}
	}
	var d workspaceDispatch
	err = pu.Runs.ReadDispatch(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, &d)
	if errors.Is(err, continuation.ErrNotFound) {
		return &workspace.Error{Code: "not_allowed", Message: "this op was not dispatched to a workspace"}
	}
	if err != nil {
		return err
	}
	if d.WorkspaceID == "" || d.WorkspaceID != workspaceID || d.Tenant != tenant {
		return &workspace.Error{Code: "not_allowed", Message: "this op was dispatched to another workspace"}
	}

	// The machine's word for how long the command ran, never longer than
	// the time since the dispatch.
	wall := res.Result.WallMS
	if elapsed := time.Since(d.DispatchedAt).Milliseconds(); wall < 0 || wall > elapsed {
		wall = max(elapsed, 0)
	}
	var pl event.Payload
	if res.ErrCode != "" {
		code := res.ErrCode
		if !errCodeRE.MatchString(code) {
			code = "provider"
		}
		pl = workspaceFailure(d.Into, workspace.ProviderAttached, computer, code, res.ErrMessage, wall)
	} else {
		pl = workspaceSuccess(d.Into, workspace.ProviderAttached, computer, "", res.Result, wall, false)
		if res.Stopped {
			pl.Raw, _ = sjson.Set(pl.Raw, d.Into+".stopped", true)
		}
	}
	recorded, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed",
		continuation.TerminalMeta{Transport: "workspace", FuelUsed: workspaceFuel(wall)}, []byte(pl.Raw))
	if err != nil {
		return err
	}
	if !recorded {
		// A repeat, or a result that arrived after the op was settled some
		// other way. The first terminal stands; this one is only noted.
		pu.Logger.Info("workspace async: a result arrived for an op that already has one",
			zap.String("run", lk.RunID), zap.String("stage", lk.Stage), zap.String("op", lk.Op))
	}
	// Whether or not THIS call recorded it: an earlier holder may have
	// recorded the terminal and died before claiming the resume, and the
	// machine sending again is what brings us here.
	return pu.ResumeIfReady(ctx, lk.RunID, lk.Stage)
}

// ResumeIfReady claims the resume of a stage whose ops have all ended and
// runs it in the background. It returns once the claim is settled — won
// here, held by another, or not yet due — and an error only when the store
// could not say. So a caller that acknowledges a worker after it returns
// nil never leaves a finished stage with nobody to resume it.
func (pu *Unit) ResumeIfReady(ctx context.Context, runID, stage string) error {
	ss, err := pu.Runs.ReadStageSuspended(ctx, runID, stage)
	if err != nil {
		return err
	}
	state, err := pu.Runs.StageState(ctx, runID, stage, ss.Manifest)
	if err != nil {
		return err
	}
	if state != continuation.StateResumable {
		return nil // a sibling op is still out
	}
	won, err := pu.Runs.ClaimResume(ctx, runID, stage)
	if err != nil || !won {
		return err
	}
	endWork := pu.Work.Begin()
	go func() {
		defer endWork()
		pu.runClaimedResume(runID, stage)
	}()
	return nil
}
