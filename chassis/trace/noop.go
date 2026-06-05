package trace

import "context"

func init() {
	Register("noop", func(StoreConfig) (Sink, error) { return NoopSink{}, nil })
}

// NoopSink is the zero-cost Sink used when TXCO_TRACE_MODE=off. Every
// method is a no-op; nothing touches the filesystem or allocates.
type NoopSink struct{}

// Begin returns a NoopTracer, which discards everything written to it.
func (NoopSink) Begin(RequestInfo) RequestTracer { return NoopTracer{} }

// Close is a no-op — there's no work to drain.
func (NoopSink) Close(context.Context) error { return nil }

// NoopTracer is the per-request side of NoopSink. Exported because
// FromContext returns it when no tracer is attached to the ctx —
// callers can compare for it if they need to short-circuit setup.
type NoopTracer struct{}

func (NoopTracer) Step(StepInfo)                           {}
func (NoopTracer) Event(TimelineEvent)                     {}
func (NoopTracer) End(status, reason string, final []byte) {}
