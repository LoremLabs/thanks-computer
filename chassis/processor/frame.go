package processor

import (
	"context"
	"sort"
	"sync"
	"time"
)

// An execution frame is one invocation of the pipeline inside a request.
//
// A request has one root frame, which the first Run builds from the inlet's
// envelope, and a call (`EXEC "call://…"`, call.go) pushes a frame of its own
// onto the same request. What belongs to the REQUEST is shared by every frame:
// the tenant and principal, the rid and tracer, the opstack snapshot, the
// fuel and TTL ledger, the deadline, the secrets, the run grant. What belongs
// to a FRAME is its own: the stack it is boxed to, its source, its input, its
// depth, its transition history, and the policies below.
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

// newCallFrame is the frame a `call://` pushes: boxed to stack, with a fresh
// transition history and the version-0 policies.
func newCallFrame(parent *frame, stack, input string, caller frameCaller) *frame {
	return &frame{
		stack:   stack,
		source:  "call",
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
