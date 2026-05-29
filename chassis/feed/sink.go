package feed

import (
	"context"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

// Sink is the producer-side seam: a chassis writes one event into the
// fleet feed and gets back the same event stamped with its
// broker-assigned ControlVersion. Mirror of Source going the other
// direction — registered the same way (init + blank import), opened
// by name from --feed-sink.
//
// The pump (chassis/controlpublish) drains the outbox and calls
// Append once per pending row. Implementations MUST use the event's
// EventID as their idempotent-publish key (Nats-Msg-Id on JetStream,
// filename on the file backend); retries with the same EventID must
// resolve to the same ControlVersion within the backend's dedup
// window. Beyond the dedup window, the consumer-side applied_events
// table is the load-bearing guard.
//
// Append is single-call-per-event from the pump's point of view; an
// error sends the row back to the pending set for retry (with
// attempt_count + last_error bookkeeping). On success the pump
// writes published_control_version + published_at back to the outbox.
type Sink interface {
	// Append publishes one event to the feed. Input has EventID
	// populated (from outbox.event_id) and ControlVersion==0. On
	// success the returned event carries the broker-assigned
	// ControlVersion; on error the input is returned unchanged so the
	// pump's diagnostic logging has the originating event_id.
	Append(ctx context.Context, e controlevent.Event) (controlevent.Event, error)

	// Name is the backend identity (for logs).
	Name() string
}

// SinkConstructor builds a Sink from resolved config. Same
// SourceConfig as the read side — backends that need richer config
// read their own env (the established seam convention; see
// chassis/continuation/factory.go and chassis/trace/factory.go).
type SinkConstructor func(SourceConfig) (Sink, error)

// sinkRegistry maps backend name → constructor. Built-in `nop` and
// `file` register from their packages' init().
var sinkRegistry = map[string]SinkConstructor{}

// RegisterSink adds a Sink backend. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func RegisterSink(name string, c SinkConstructor) {
	sinkRegistry[name] = c
}

// OpenSink constructs the named Sink backend. Unknown name is a
// startup error listing what is available.
func OpenSink(name string, cfg SourceConfig) (Sink, error) {
	c, ok := sinkRegistry[name]
	if !ok {
		avail := make([]string, 0, len(sinkRegistry))
		for k := range sinkRegistry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("feed: unknown sink %q (available: %v)", name, avail)
	}
	return c(cfg)
}
