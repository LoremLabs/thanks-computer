package processor

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// LiveRuns is the registry of the runs in flight in this process, so a person
// (or a stack) can name one and end it without ending the chassis. The
// server's bus loop registers each request goroutine's run on entry and
// removes it on return; the processor's pin sites keep the entry current —
// the tenant (WithTenant), the stack the run was routed into (maybeRetenant)
// and the scope running now (Run, at dispatch).
//
// An abort is a context cancel with an AbortError as its cause. The scope
// loop's existing cancel branch takes it exactly as it takes a client
// disconnect or a deadline — every op still in flight is flushed as a step
// and nothing later in the run executes — and records the run as `aborted`,
// with who asked, rather than `cancelled`: a client going away is a
// different fact. A hard stop: there is no cleanup hook. A stack that must
// tidy up does it before it asks.
//
// Nil-safe, so a Unit built without New (unit tests) tracks nothing.
type LiveRuns struct {
	mu   sync.Mutex
	runs map[string]*liveRun
}

// liveRun is one registered run. The identity fields are set once at
// Register; the pinned fields move as the run does, under the entry's own
// lock, so the registry lock is held only to look an entry up. A run has one
// holder at first (its request goroutine) and gains one per piece of work
// that outlives the request and attaches (a continuable op's detached exec);
// an abort cancels every holder's context, and the entry stays until the
// last holder is done.
type liveRun struct {
	rid     string
	src     string
	started time.Time

	mu      sync.Mutex
	cancels []context.CancelCauseFunc
	holders int
	tenant  string
	entry   string // the stack the run was routed into
	stack   string // the stack of the scope running now
	stage   string // "<stack>/<scope>" of the scope running now
	aborted *AbortError
}

// LiveRun is one run as List reports it.
type LiveRun struct {
	RID     string    `json:"rid"`
	Tenant  string    `json:"tenant"`
	Src     string    `json:"src"`
	Entry   string    `json:"entry,omitempty"`
	Stack   string    `json:"stack,omitempty"`
	Stage   string    `json:"stage,omitempty"`
	Started time.Time `json:"started"`
	// AbortedBy names who asked for the abort when one is in progress — the
	// run is still listed until its goroutine returns.
	AbortedBy string `json:"aborted_by,omitempty"`
}

// AbortError is the cause an aborted run's context carries: who asked, and
// why. The scope loop reads it to record the run's reason.
type AbortError struct {
	By     string
	Reason string
}

func (e *AbortError) Error() string {
	s := "aborted"
	if e.By != "" {
		s += " by " + e.By
	}
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

// NewLiveRuns returns an empty registry.
func NewLiveRuns() *LiveRuns { return &LiveRuns{runs: map[string]*liveRun{}} }

// ctxKeyLiveRun carries a run's registry entry so the processor's pin sites
// can keep it current.
var ctxKeyLiveRun = ctxKeyType{name: "live-run"}

func liveRunFromContext(ctx context.Context) *liveRun {
	lr, _ := ctx.Value(ctxKeyLiveRun).(*liveRun)
	return lr
}

// Register enters a run under its rid and returns the context the run must
// execute on — cancellable by Abort — and the func that releases the entry
// when the run returns (call it once; more is a no-op). A second
// registration of the same rid replaces the first in the registry (an inlet
// that takes the rid from its client cannot promise uniqueness); the first
// run keeps running and releases only itself.
func (r *LiveRuns) Register(ctx context.Context, rid, src string) (context.Context, func()) {
	if r == nil || rid == "" {
		return ctx, func() {}
	}
	lr := &liveRun{rid: rid, src: src, started: time.Now()}
	r.mu.Lock()
	r.runs[rid] = lr
	r.mu.Unlock()
	return r.hold(ctx, lr)
}

// Attach joins work that outlives its request goroutine — a continuable
// op's detached exec, which runs on a context of its own — to the run's
// entry under the same rid, so an abort of the run reaches it after the
// request has answered its client. The entry is the request's when it is
// still registered, or a new one (a resumed run) pinned to the tenant on
// ctx. The returned context is cancelled when the run is aborted; the func
// releases the attachment. Work attached to a run already aborted is
// cancelled at once.
func (r *LiveRuns) Attach(ctx context.Context, rid, src string) (context.Context, func()) {
	if r == nil || rid == "" {
		return ctx, func() {}
	}
	r.mu.Lock()
	lr := r.runs[rid]
	if lr == nil {
		lr = &liveRun{rid: rid, src: src, started: time.Now(), tenant: tenantScope(ctx)}
		r.runs[rid] = lr
	}
	r.mu.Unlock()
	return r.hold(ctx, lr)
}

// hold gives one holder of the entry its cancellable context and release.
func (r *LiveRuns) hold(ctx context.Context, lr *liveRun) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	lr.mu.Lock()
	lr.cancels = append(lr.cancels, cancel)
	lr.holders++
	aborted := lr.aborted
	lr.mu.Unlock()
	if aborted != nil {
		cancel(aborted)
	}
	var once sync.Once
	done := func() {
		once.Do(func() {
			lr.mu.Lock()
			lr.holders--
			last := lr.holders == 0
			lr.mu.Unlock()
			if last {
				r.mu.Lock()
				if r.runs[lr.rid] == lr {
					delete(r.runs, lr.rid)
				}
				r.mu.Unlock()
			}
			cancel(nil)
		})
	}
	return context.WithValue(ctx, ctxKeyLiveRun, lr), done
}

// Count is the number of runs registered now.
func (r *LiveRuns) Count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

// List reports the live runs of a tenant, oldest first; an empty tenant
// reports every tenant's (the super-admin's view).
func (r *LiveRuns) List(tenant string) []LiveRun {
	out := []LiveRun{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	entries := make([]*liveRun, 0, len(r.runs))
	for _, lr := range r.runs {
		entries = append(entries, lr)
	}
	r.mu.Unlock()
	for _, lr := range entries {
		s := lr.snapshot()
		if tenant != "" && s.Tenant != tenant {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Started.Equal(out[j].Started) {
			return out[i].RID < out[j].RID
		}
		return out[i].Started.Before(out[j].Started)
	})
	return out
}

// Abort ends one run: its context is cancelled with the AbortError as the
// cause. False when no run has the rid, or it belongs to another tenant
// (an empty tenant matches any). Aborting a run twice is a no-op that
// still answers true.
func (r *LiveRuns) Abort(tenant, rid, by, reason string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	lr := r.runs[rid]
	r.mu.Unlock()
	if lr == nil {
		return false
	}
	return lr.abort(tenant, "", by, reason)
}

// AbortStack ends every live run of a tenant that was routed into the stack
// or is running one of its scopes now, and reports how many.
func (r *LiveRuns) AbortStack(tenant, stack, by, reason string) int {
	if r == nil || stack == "" {
		return 0
	}
	r.mu.Lock()
	entries := make([]*liveRun, 0, len(r.runs))
	for _, lr := range r.runs {
		entries = append(entries, lr)
	}
	r.mu.Unlock()
	n := 0
	for _, lr := range entries {
		if lr.abort(tenant, stack, by, reason) {
			n++
		}
	}
	return n
}

// abort cancels the run when it matches: the tenant (empty matches any) and,
// when a stack is named, the stack it entered at or is in now.
func (lr *liveRun) abort(tenant, stack, by, reason string) bool {
	lr.mu.Lock()
	if tenant != "" && lr.tenant != tenant {
		lr.mu.Unlock()
		return false
	}
	if stack != "" && lr.entry != stack && lr.stack != stack {
		lr.mu.Unlock()
		return false
	}
	if lr.aborted == nil {
		lr.aborted = &AbortError{By: by, Reason: reason}
	}
	cause := lr.aborted
	cancels := append([]context.CancelCauseFunc(nil), lr.cancels...)
	lr.mu.Unlock()
	for _, cancel := range cancels {
		cancel(cause)
	}
	return true
}

func (lr *liveRun) snapshot() LiveRun {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	s := LiveRun{
		RID: lr.rid, Tenant: lr.tenant, Src: lr.src,
		Entry: lr.entry, Stack: lr.stack, Stage: lr.stage, Started: lr.started,
	}
	if lr.aborted != nil {
		s.AbortedBy = lr.aborted.By
	}
	return s
}

// setTenant records the tenant the run is pinned to. Nil-safe.
func (lr *liveRun) setTenant(slug string) {
	if lr == nil {
		return
	}
	lr.mu.Lock()
	lr.tenant = slug
	lr.mu.Unlock()
}

// SetLiveRunEntry records the stack the run on ctx was routed into — what
// the boot handoff records itself (maybeRetenant). Exposed for a caller
// that routes a run without the handoff, and for tests in other packages.
func SetLiveRunEntry(ctx context.Context, stack string) {
	liveRunFromContext(ctx).setEntry(stack)
}

// setEntry records the stack the run was routed into. Nil-safe.
func (lr *liveRun) setEntry(stack string) {
	if lr == nil || stack == "" {
		return
	}
	lr.mu.Lock()
	lr.entry = stack
	lr.mu.Unlock()
}

// setStage records the scope the run is dispatching now. Nil-safe.
func (lr *liveRun) setStage(stack, stage string) {
	if lr == nil {
		return
	}
	lr.mu.Lock()
	lr.stack, lr.stage = stack, stage
	lr.mu.Unlock()
}

// AbortCause reports the AbortError a context was cancelled with, when it
// was an abort (and not a deadline, a client disconnect or shutdown). An
// op's own timeout context inherits its parent's cause, so this reads true
// from inside an op as well as from the scope loop.
func AbortCause(ctx context.Context) (*AbortError, bool) {
	var ab *AbortError
	if errors.As(context.Cause(ctx), &ab) {
		return ab, true
	}
	return nil, false
}
