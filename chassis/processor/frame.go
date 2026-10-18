package processor

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// An execution frame is one invocation of the pipeline inside a request.
//
// A request has one root frame, which the first Run builds from the inlet's
// envelope, and a call (`EXEC "call://…"`, call.go) pushes a frame of its own
// onto the same request. What belongs to the REQUEST is shared by every frame:
// the tenant and principal, the rid and tracer, the opstack snapshot, the
// fuel ledger, the deadline, the secrets, the run grant (unless the call
// names one). What belongs to a FRAME is its own: the stack it is boxed to,
// its source, its input, its depth, its transition history, its hop
// countdown, and the policies below.
//
// The frame decides only where a call differs. The root frame's every policy
// says "as before", and each check that reads a frame treats nil and root
// alike, so a request without a call runs the code that existed before frames
// did — call_oracle_test.go holds it to that, byte for byte.
type frame struct {
	// stack is the frame's boundary: the stack a call runs, and only it.
	// "" on the root frame, which is not boxed: its lookups peel into parent
	// stacks and its jumps go anywhere in the tenant.
	stack string
	// source is what `_txc.src` was pinned to for this frame: the inlet's at
	// the root, "call" for a call.
	source string
	// depth is 0 at the root and one more per nested call.
	depth int
	// input is the call's resolved `WITH input` (JSON); "{}" at the root.
	input  string
	caller frameCaller

	// Policies. The root frame allows everything; a call frame allows none of
	// these (design §2.2, §2.7).
	peels       bool // may a stage lookup fall back to a parent stack
	mayStream   bool // may a scope flush a body chunk to the client
	mayContinue bool // may a scope suspend (WITH mode = async / continuable)

	// seen is the frame's own repeat-transition history (§2.8), so a second
	// call of the same stack in one request pays no repeat penalty. nil on
	// the root frame, which keeps using the budget's.
	seenMu sync.Mutex
	seen   map[string]bool
	// ttl is the frame's own hop countdown (budget.go decrementTTL): a
	// call frame starts at --op-scope-ttl-max, as the request did, so a
	// sub-flow pays for its own loops and the request never for the
	// sub-flow's hops — a capability answered in process walks its whole
	// declaring stack, which would otherwise spend the request's. Depth
	// bounds the total. Unused on the root frame, which counts on the
	// budget's.
	ttl atomic.Int64

	// Ops in flight, so a call can wait for its callee's ops to stop on
	// cancellation (§2.9). Counted on call frames only.
	opsMu   sync.Mutex
	running map[string]int
	ops     sync.WaitGroup

	// ctx is the call's own context, cancelled when the call returns, so an
	// op goroutine that outlives the callee's Run can tell nobody is reading
	// its answer. nil at the root.
	ctx context.Context

	// fail is the first failure a boundary or policy check recorded. The
	// call engine answers it in place of whatever the callee emitted.
	failMu sync.Mutex
	fail   *callError
}

// frameCaller names the rule that made a call, as the callee sees it at
// `_txc.call.caller`.
type frameCaller struct {
	Stack string `json:"stack"`
	Stage string `json:"stage"`
	Op    string `json:"op"`
}

var ctxKeyFrame = ctxKeyType{name: "frame"}

func withFrame(ctx context.Context, f *frame) context.Context {
	return context.WithValue(ctx, ctxKeyFrame, f)
}

// frameFrom returns the frame the context runs in, or nil for a context that
// never went through a first Run: tests that drive pieces directly, and
// resumes and detached work until they build frames too. nil behaves as the
// root frame everywhere.
func frameFrom(ctx context.Context) *frame {
	f, _ := ctx.Value(ctxKeyFrame).(*frame)
	return f
}

// rootFrame is the frame of the inlet's own run.
func rootFrame(source string) *frame {
	return &frame{source: source, peels: true, mayStream: true, mayContinue: true}
}

// newCallFrame is the frame a call pushes (a `call://`, or a capability
// answered on this chassis): boxed to stack, under source, with a fresh
// transition history and the version-0 policies.
func newCallFrame(parent *frame, stack, source, input string, caller frameCaller) *frame {
	return &frame{
		stack:   stack,
		source:  source,
		depth:   parent.callDepth() + 1,
		input:   input,
		caller:  caller,
		seen:    map[string]bool{},
		running: map[string]int{},
	}
}

// isCall reports whether f is a call frame. nil and the root are not.
func (f *frame) isCall() bool { return f != nil && f.depth > 0 }

// callDepth is how many calls deep this frame runs: 0 for nil and the root.
func (f *frame) callDepth() int {
	if f == nil {
		return 0
	}
	return f.depth
}

// mayPeel reports whether a stage lookup under this frame may fall back to a
// parent stack. Only a call frame says no.
func (f *frame) mayPeel() bool { return f == nil || f.peels }

// confines reports whether stage lies inside the frame's boundary. Every
// stage is inside the root's; a call's holds only its own stack's.
func (f *frame) confines(stage string) bool {
	if !f.isCall() {
		return true
	}
	m := StagePartsRE.FindStringSubmatch(stage)
	return len(m) == 3 && m[1] == f.stack
}

// sawTransition records a transition in the frame's own history and reports
// whether this frame had taken it before.
func (f *frame) sawTransition(key string) bool {
	f.seenMu.Lock()
	defer f.seenMu.Unlock()
	repeat := f.seen[key]
	f.seen[key] = true
	return repeat
}

// opStarted and opDone bracket one op goroutine of a call frame. The root
// frame counts nothing: its ops are the request's, and the request does not
// wait for them.
func (f *frame) opStarted(label string) {
	if !f.isCall() {
		return
	}
	f.ops.Add(1)
	f.opsMu.Lock()
	f.running[label]++
	f.opsMu.Unlock()
}

func (f *frame) opDone(label string) {
	if !f.isCall() {
		return
	}
	f.opsMu.Lock()
	if f.running[label] > 1 {
		f.running[label]--
	} else {
		delete(f.running, label)
	}
	f.opsMu.Unlock()
	f.ops.Done()
}

// stillRunning names the ops the frame started that have not returned.
func (f *frame) stillRunning() []string {
	f.opsMu.Lock()
	defer f.opsMu.Unlock()
	out := make([]string, 0, len(f.running))
	for label := range f.running {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// waitOps waits up to grace for the frame's ops in flight to return. It is
// called once the callee's Run has returned, so no op can start after it;
// stopped is false when some op outlived the grace, and running names them.
func (f *frame) waitOps(grace time.Duration) (stopped bool, running []string) {
	done := make(chan struct{})
	go func() {
		f.ops.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true, nil
	case <-time.After(grace):
		return false, f.stillRunning()
	}
}

// failWith records a failure of the frame. The first one stands: it is the
// cause, and what follows is its consequence.
func (f *frame) failWith(code, message string) {
	if !f.isCall() {
		return
	}
	f.failMu.Lock()
	defer f.failMu.Unlock()
	if f.fail == nil {
		f.fail = &callError{Code: code, Message: message}
	}
}

// failure is the frame's recorded failure, or nil.
func (f *frame) failure() *callError {
	if f == nil {
		return nil
	}
	f.failMu.Lock()
	defer f.failMu.Unlock()
	if f.fail == nil {
		return nil
	}
	cp := *f.fail
	return &cp
}

// resumeFrame is the frame a resumed continuation runs in: the request's
// own policies, except that nobody is on the other end of its channel, so
// it cannot stream.
func resumeFrame(source string) *frame {
	return &frame{source: source, peels: true, mayContinue: true}
}

// detachedFrame is the frame a detached op runs in — a promoted continuable
// op, a local async op — on a context of its own: no client to stream to.
func detachedFrame(source string) *frame {
	return &frame{source: source, peels: true, mayContinue: true}
}

// resumeContext builds the context a resumed continuation runs under, from
// the chassis-stamped envelope captured at suspend: the tenant, the source
// and the principal re-pinned from it — never from merged worker output,
// which a hostile worker could have shaped — the resume identity, and a
// frame that cannot stream. Run's first-Run block is skipped once the
// snapshot is attached, so everything it would have pinned is pinned here,
// in one place for both resume paths.
func (pu *Unit) resumeContext(ctx context.Context, scopeEnvelope string, ident resumeIdent) context.Context {
	rctx := withResumeRun(ctx, ident)
	rctx = WithTenant(rctx, gjson.Get(scopeEnvelope, "_txc.tenant").String())
	src := gjson.Get(scopeEnvelope, "_txc.src").String()
	rctx = WithSource(rctx, src)
	rctx = withPrincipalFrom(rctx, scopeEnvelope)
	return withFrame(rctx, resumeFrame(src))
}

// attachResumeSnapshot pins the opstack frozen at suspend onto rctx, so
// every lookup of the resumed execution — this stage, the next, any deeper
// advancement or re-suspend — resolves against it rather than the live
// opstack. No snapshot, or one that cannot be rebuilt, leaves the live
// opstack in place (pre-snapshot runs, untenanted paths). The returned
// release closes the snapshot; call it when the resume returns.
func (pu *Unit) attachResumeSnapshot(ctx, rctx context.Context, runID string) (context.Context, func()) {
	snapData, serr := pu.Runs.ReadOpstackSnapshot(ctx, runID)
	if serr != nil || len(snapData) == 0 {
		return rctx, func() {}
	}
	snapDB, berr := buildSnapshotDB(snapData)
	if berr != nil {
		pu.Logger.Warn("opstack snapshot rebuild failed; resuming against live opstack",
			zap.String("run", runID), zap.Error(berr))
		return rctx, func() {}
	}
	return context.WithValue(rctx, ctxKeyOpstackSnap, snapDB), func() { snapDB.Close() }
}
