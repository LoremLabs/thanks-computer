package processor

import (
	"context"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// detachedTraceSlack is added to a detached op's own timeout to give the idle
// allowance of the trace that holds it: long enough that the op's deadline,
// not the sink, ends a quiet trace.
const detachedTraceSlack = 5 * time.Minute

// ResumeTraceOptions are what differ between the ways a run resumes.
type ResumeTraceOptions struct {
	// StartedAt is when the trace begins; zero means now. A detached op's
	// trace holds the op itself, so it begins when the op did.
	StartedAt time.Time
	// IdleTTL is RequestInfo.IdleTTL: how long the trace may sit quiet.
	IdleTTL time.Duration
}

// BeginResumeTrace opens the trace of one resumed stage: every way a run comes
// back goes through here, so each gets what a trace is owed. The tenant, so a
// stack's hints find their slot; the suspended stack's hints from the first
// record; the by-value scrub of issued secrets; the event that links the
// trace to the request that suspended.
//
// It answers ctx carrying the issued-secret list and the tracer, the tracer,
// and the run's tenant for EmitUsage. A nil sink means no trace at all, which
// is a nil tracer. Scrubbing a trace nothing writes would hide from the
// caller that tracing is off, so a NoopTracer stays one.
func (pu *Unit) BeginResumeTrace(ctx context.Context, sink trace.Sink, runID, stage string, o ResumeTraceOptions) (context.Context, trace.RequestTracer, string) {
	if sink == nil {
		return ctx, nil, ""
	}
	ctx = WithIssuedSecrets(ctx)
	started := o.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	rc, rcErr := pu.Runs.ReadRunCreated(ctx, runID)
	var tenant string
	if rcErr == nil {
		tenant = rc.TenantID
	}
	stack, _, _ := pu.StageParse(stage)

	tracer := sink.Begin(trace.RequestInfo{
		RID:         continuation.ResumeTraceRID(runID, stage),
		Src:         "continuation",
		Tenant:      tenant,
		Stack:       stage,
		StartedAt:   started,
		IdleTTL:     o.IdleTTL,
		RedactStack: stack,
	})
	if _, noop := tracer.(trace.NoopTracer); !noop {
		tracer = ScrubbingTracer(ctx, tracer)
	}
	// Linkage event: admin-ui cross-navigates between the suspending request
	// and this trace with it.
	if rcErr == nil {
		tracer.Event(trace.TimelineEvent{
			Ts:    time.Now(),
			Event: "continuation.resume",
			Fields: map[string]any{
				"run_id":              runID,
				"run_continuation_id": rc.RunContinuationID,
				"origin_rid":          rc.OriginRID,
				"stage":               stage,
				"stack_version_id":    rc.StackVersionID,
			},
		})
	}
	return trace.WithContext(ctx, tracer), tracer, tenant
}
