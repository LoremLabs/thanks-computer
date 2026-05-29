package feed

import "context"

// Acker is an optional interface a Source implements when its events
// need explicit acknowledgement after a successful local apply. The
// applier calls Ack(eventID) only after the SQLite tx that wrote
// applied_events + the data rows + the cursor advance commits — so
// a crash between fetch and ack causes the broker to redeliver, and
// the consumer-side applied_events guard makes the replay a no-op.
//
// Sources that don't need per-event acks (e.g. the built-in file
// source, which reads from a directory and has no broker-side cursor)
// simply don't implement this interface. The applier type-asserts
// and falls back to a no-op when absent.
//
// JetStream pull consumers in the service overlay are the primary
// implementor today (overlay/nats/controlfeed/source.go): they retain
// the per-message ack handle keyed by EventID until the applier
// confirms apply, then `AckSync` against the broker.
type Acker interface {
	Ack(ctx context.Context, eventID string) error
}
