package trace

import (
	"sync"
	"sync/atomic"
)

// Relay is the tracer of work that starts before anyone knows which trace it
// belongs to. A `mode = "continuable"` op starts first and races its timer:
// win, and its steps belong to the request; lose, and they belong to a resume
// trace that does not exist until the run has been minted. The relay holds
// what the work records until that is settled (FlushTo), then forwards.
//
// Close ends the forwarding, and every End of a target comes after it: a
// timed-out op may still be unwinding, and a step written to a trace that has
// ended goes to a closed file. Anything later is dropped.
//
// The ceiling bounds the bodies one relay passes on. Past it a step keeps its
// sizes and timings and loses its bodies; whole or not at all, because a
// stack's redaction hints need whole JSON. 0 keeps metadata only.
//
// All methods are safe for concurrent use, as RequestTracer requires, and on
// a nil *Relay, which is the relay of work no trace is watching.
type Relay struct {
	mu      sync.RWMutex // held for reading across a forward, so Close waits for it
	buf     []relayed
	target  RequestTracer
	closed  bool
	ceiling int64
	kept    atomic.Int64
}

// relayed is one held record, in the order it arrived.
type relayed struct {
	step  *StepInfo
	event *TimelineEvent
}

// NewRelay returns a relay that passes on at most ceiling bytes of bodies.
func NewRelay(ceiling int64) *Relay {
	if ceiling < 0 {
		ceiling = 0
	}
	return &Relay{ceiling: ceiling}
}

// Step holds or forwards one step, after the ceiling.
func (r *Relay) Step(info StepInfo) {
	if r == nil {
		return
	}
	r.admit(&info)

	r.mu.RLock()
	if r.target != nil {
		r.target.Step(info)
		r.mu.RUnlock()
		return
	}
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.closed:
	case r.target != nil:
		r.target.Step(info) // settled between the two locks
	default:
		r.buf = append(r.buf, relayed{step: &info})
	}
}

// Event holds or forwards one timeline event.
func (r *Relay) Event(ev TimelineEvent) {
	if r == nil {
		return
	}
	r.mu.RLock()
	if r.target != nil {
		r.target.Event(ev)
		r.mu.RUnlock()
		return
	}
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.closed:
	case r.target != nil:
		r.target.Event(ev)
	default:
		r.buf = append(r.buf, relayed{event: &ev})
	}
}

// End does nothing: the relay never owns the trace it forwards to.
func (r *Relay) End(string, string, []byte) {}

// FlushTo replays what is held to t, in order, and forwards to it from then
// on. A nil t means no trace will have this work: the relay closes. Only the
// first call counts.
func (r *Relay) FlushTo(t RequestTracer) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.target != nil {
		return
	}
	if t == nil {
		r.closed, r.buf = true, nil
		return
	}
	for _, rec := range r.buf {
		switch {
		case rec.step != nil:
			t.Step(*rec.step)
		case rec.event != nil:
			t.Event(*rec.event)
		}
	}
	r.buf, r.target = nil, t
}

// Close ends the relay, waiting for any forward in progress. Anything held
// and not flushed is dropped.
func (r *Relay) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed, r.buf, r.target = true, nil, nil
}

// admit applies the ceiling. A step over it keeps its true sizes, which every
// sink and reader turn into input_truncated and output_truncated. Past the
// ceiling a smaller body may still fit.
func (r *Relay) admit(info *StepInfo) {
	n := int64(len(info.Input) + len(info.Output))
	if n == 0 {
		return
	}
	if r.kept.Add(n) <= r.ceiling {
		return
	}
	r.kept.Add(-n)
	if info.InputBytes == 0 {
		info.InputBytes = len(info.Input)
	}
	if info.OutputBytes == 0 {
		info.OutputBytes = len(info.Output)
	}
	info.Input, info.Output = nil, nil
}
