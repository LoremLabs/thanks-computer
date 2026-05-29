package trace

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AsyncOpts configures an AsyncSink.
//
//	BufferSize    — channel capacity. When full, new events are dropped
//	                rather than blocking the request path. Default 1024.
//	BodyCapBytes  — maximum bytes kept per body (Input, Output, Payload,
//	                final). Larger payloads are truncated; meta.json
//	                records the original size and a `*_truncated` flag.
//	                0 disables capping. Default 65536.
//	StaleAfter    — idle TTL for per-request tracers held in the worker's
//	                map. If a tracer hasn't received Step/Event/End for
//	                this long, the worker closes it to avoid leaked file
//	                descriptors when End events drop under load. Default
//	                60s.
//	DropCounter   — optional atomic counter incremented when the buffer
//	                is full and an event is dropped. nil to ignore.
type AsyncOpts struct {
	BufferSize   int
	BodyCapBytes int
	StaleAfter   time.Duration
	DropCounter  *atomic.Int64
}

// AsyncSink wraps another Sink with a worker goroutine and a buffered
// channel. Every per-request method (Begin, Step, Event, End) goes
// through the channel — the request path NEVER touches the disk and
// returns within a few microseconds whether tracing is on or off.
//
// The worker drains the channel in order, looks each event up in its
// tracer map by rid, and forwards to the per-request tracer it got
// from base.Begin. When an End arrives, the worker spawns a flush
// goroutine so it doesn't sit blocked through the per-request disk
// burst — many flushes can run in parallel, and the worker returns
// to draining the channel immediately.
//
// Per-request ordering is preserved by the FIFO channel: all events
// from one request originate in the same request goroutine and are
// enqueued in order.
//
// Drop semantics under overload:
//
//   - If a Begin enqueue drops, the tracer never gets created; all
//     subsequent Step/Event/End for that rid silently no-op in the
//     worker. Trace for that request is missing entirely (better than
//     half-formed).
//   - If an End enqueue drops AFTER a Begin landed, the tracer would
//     linger in the worker's map. The ticker-based GC closes any
//     tracer idle longer than StaleAfter so the leak is bounded.
type AsyncSink struct {
	base    Sink
	opts    AsyncOpts
	ch      chan asyncOp
	wg      sync.WaitGroup
	flushWg sync.WaitGroup // tracks End-flush goroutines for shutdown drain
	closed  atomic.Bool
}

type asyncOpKind int

const (
	opBegin asyncOpKind = iota
	opStep
	opEvent
	opEnd
)

type asyncOp struct {
	kind   asyncOpKind
	rid    string // routes to the right per-request tracer in the worker's map
	info   RequestInfo
	step   StepInfo
	event  TimelineEvent
	status string
	final  []byte
}

// NewAsyncSink wraps base with the given options. The worker
// goroutine starts immediately.
func NewAsyncSink(base Sink, opts AsyncOpts) *AsyncSink {
	if opts.BufferSize <= 0 {
		opts.BufferSize = 1024
	}
	if opts.BodyCapBytes < 0 {
		opts.BodyCapBytes = 0
	} else if opts.BodyCapBytes == 0 {
		opts.BodyCapBytes = 65536
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 60 * time.Second
	}

	s := &AsyncSink{
		base: base,
		opts: opts,
		ch:   make(chan asyncOp, opts.BufferSize),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Begin enqueues a begin event and returns an asyncTracer keyed by the
// request rid. The actual base.Begin call happens in the worker —
// dir creation, in.json write, and timeline.jsonl open all run off
// the request path so request latency is independent of disk speed.
//
// If the channel is full when Begin enqueues, the tracer is silently
// dropped: the request continues normally (this isn't a request error),
// but no trace artifacts will be written for it. Better than producing
// a partial trace.
func (s *AsyncSink) Begin(info RequestInfo) RequestTracer {
	if cap := s.opts.BodyCapBytes; cap > 0 && len(info.Payload) > cap {
		info.PayloadBytes = len(info.Payload)
		info.Payload = info.Payload[:cap]
	}
	if !s.tryEnqueue(asyncOp{kind: opBegin, rid: info.RID, info: info}) {
		return NoopTracer{}
	}
	return &asyncTracer{sink: s, rid: info.RID}
}

// Close stops accepting new work, drains pending work, waits for any
// in-flight flush goroutines, and closes the underlying base sink.
// Returns ctx.Err() if the drain doesn't complete before ctx is
// cancelled.
func (s *AsyncSink) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.ch)

	done := make(chan struct{})
	go func() {
		s.wg.Wait()      // worker drained and exited
		s.flushWg.Wait() // outstanding parallel flushes finished
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.base.Close(ctx)
}

// DroppedCount returns the number of events dropped because the
// buffer was full (or zero if no DropCounter was configured).
func (s *AsyncSink) DroppedCount() int64 {
	if s.opts.DropCounter == nil {
		return 0
	}
	return s.opts.DropCounter.Load()
}

// trackedTracer is the worker-local record of a live per-request
// tracer; lastActive feeds the GC ticker.
type trackedTracer struct {
	tracer     RequestTracer
	lastActive time.Time
}

// run is the worker goroutine. It drains the event channel, maintains
// a map of per-request tracers keyed by rid, and periodically closes
// stale tracers so a dropped End doesn't leak file descriptors.
func (s *AsyncSink) run() {
	defer s.wg.Done()
	tracers := map[string]*trackedTracer{}
	ticker := time.NewTicker(s.opts.StaleAfter / 2)
	defer ticker.Stop()

	for {
		select {
		case op, ok := <-s.ch:
			if !ok {
				// Channel closed and drained. Anything still in the map
				// is a stranded tracer (its End never made it through).
				// Flush each in a goroutine so all interrupted traces
				// land in parallel; Close.flushWg.Wait gates shutdown.
				for _, tt := range tracers {
					s.flushWg.Add(1)
					go func(tracer RequestTracer) {
						defer s.flushWg.Done()
						tracer.End("interrupted", nil)
					}(tt.tracer)
				}
				return
			}
			now := time.Now()
			switch op.kind {
			case opBegin:
				// Create the per-request tracer via the base sink.
				// This is the moment the request directory is mkdir'd
				// and in.json gets written.
				tracers[op.rid] = &trackedTracer{
					tracer:     s.base.Begin(op.info),
					lastActive: now,
				}
			case opStep:
				if tt, ok := tracers[op.rid]; ok {
					tt.tracer.Step(op.step)
					tt.lastActive = now
				}
			case opEvent:
				if tt, ok := tracers[op.rid]; ok {
					tt.tracer.Event(op.event)
					tt.lastActive = now
				}
			case opEnd:
				if tt, ok := tracers[op.rid]; ok {
					tt.tracer.End(op.status, op.final)
					delete(tracers, op.rid)
				}
			}
		case <-ticker.C:
			// GC tracers whose End never arrived: under sustained
			// overload an enqueued End can drop. We close stale
			// entries with status="stale" to bound the leak. Flush in
			// a goroutine so the worker isn't blocked.
			now := time.Now()
			for rid, tt := range tracers {
				if now.Sub(tt.lastActive) > s.opts.StaleAfter {
					tracer := tt.tracer
					delete(tracers, rid)
					s.flushWg.Add(1)
					go func() {
						defer s.flushWg.Done()
						tracer.End("stale", nil)
					}()
				}
			}
		}
	}
}

// tryEnqueue is the back end of every tracer method (and Begin). Non-
// blocking: if the channel is full, the event is dropped and the
// optional drop counter is incremented. Dropping is safe — traces are
// observability, not correctness; losing some under sustained overload
// is better than back-pressuring the request path.
func (s *AsyncSink) tryEnqueue(op asyncOp) bool {
	if s.closed.Load() {
		return false
	}
	select {
	case s.ch <- op:
		return true
	default:
		if s.opts.DropCounter != nil {
			s.opts.DropCounter.Add(1)
		}
		return false
	}
}

// asyncTracer is the per-request handle the chassis sees. It holds the
// rid only — the actual RequestTracer lives in the worker's map. All
// methods enqueue events tagged with the rid and return immediately.
type asyncTracer struct {
	sink *AsyncSink
	rid  string
}

func (t *asyncTracer) Step(info StepInfo) {
	if cap := t.sink.opts.BodyCapBytes; cap > 0 {
		if len(info.Input) > cap {
			info.InputBytes = len(info.Input)
			info.Input = info.Input[:cap]
		}
		if len(info.Output) > cap {
			info.OutputBytes = len(info.Output)
			info.Output = info.Output[:cap]
		}
	}
	t.sink.tryEnqueue(asyncOp{kind: opStep, rid: t.rid, step: info})
}

func (t *asyncTracer) Event(ev TimelineEvent) {
	t.sink.tryEnqueue(asyncOp{kind: opEvent, rid: t.rid, event: ev})
}

func (t *asyncTracer) End(status string, final []byte) {
	if cap := t.sink.opts.BodyCapBytes; cap > 0 && len(final) > cap {
		final = final[:cap]
	}
	t.sink.tryEnqueue(asyncOp{kind: opEnd, rid: t.rid, status: status, final: final})
}
