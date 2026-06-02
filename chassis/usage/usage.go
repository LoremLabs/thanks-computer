// Package usage emits one append-only event per completed request — the
// raw material a future control plane aggregates into per-tenant counts,
// quotas, and billing. This slice is log-only: no counting, no
// enforcement, no entitlement state. A request finishes, we record what
// we already know at the convergence point (rid, tenant, sizes, timing,
// status), and something downstream derives everything else.
//
// The package is interface-first so the transport is swappable without
// touching call sites. The default ZapSink writes a structured
// "usage" line through the chassis's existing zap logger; a later
// file / Kafka / NATS / OTEL sink drops in behind the same Sink.
package usage

import (
	"context"

	"go.uber.org/zap"
)

// UsageEvent is the per-request record. Deliberately flat and
// derived-metric-free: ops counts, monthly rollups, and rate-limit
// inputs are a downstream consumer's job, not the runtime's.
//
// Tenant is the resolved tenant slug when routing succeeded; it is
// "_sys" (or empty) for unrouted/404 traffic, which is logged as-is so
// rejected requests are still measured.
type UsageEvent struct {
	RID        string
	Tenant     string
	Src        string // "http" | "tcp" | "cron"
	Stack      string // entry stage the request dispatched to
	DurationMS int64
	Status     string // "ok" | "error"
	BytesIn    int
	BytesOut   int
	// MemBytes is peak guest memory for a compute invocation (src="compute").
	// Zero for request-level usage (http/tcp/cron have no wasm memory) and
	// omitted from the log line in that case.
	MemBytes int
	// Fuel is the total per-request fuel consumed across accounted actions
	// (scope-enter, repeat-transition, EXEC, secret-materialize in v1;
	// finer-grained costs follow in v2). Zero on requests that did no
	// accounted work (empty / unrouted), in which case it is omitted from
	// the log line — mirrors the MemBytes conditional pattern. This is the
	// metering primitive; per-request enforcement happens upstream via the
	// chassis budget guards and is invisible to the sink.
	Fuel int64

	// AdmissionDenied marks a request the admission gate rejected (suspend,
	// rate limit, concurrency, or drain) before the customer stack ran. For
	// such requests Billable is false and Fuel is zeroed, so a downstream
	// aggregator never counts rejected traffic as usage. AdmissionReason is
	// the machine token ("rate_limited" | "at_capacity" | "suspended" |
	// "payment_required" | "draining").
	AdmissionDenied bool
	AdmissionReason string
	Billable        bool
}

// Sink consumes usage events. WriteEvent must be safe for concurrent
// calls (the bus loop emits from per-request goroutines). Close is
// called once on chassis shutdown; synchronous sinks return nil
// immediately.
type Sink interface {
	WriteEvent(ev UsageEvent)
	Close(ctx context.Context) error
}

// ZapSink is the bundled default sink: it folds the event into a
// single structured log line. The stable "usage" message is the
// downstream filter key; the timestamp is zap's own. Close is a no-op —
// the logger's flush lifecycle is owned by the chassis, not here.
type ZapSink struct {
	log *zap.Logger
}

// NewZapSink wraps an existing chassis logger. The logger is expected
// to be non-nil; callers only construct a ZapSink when usage is enabled.
func NewZapSink(log *zap.Logger) *ZapSink {
	return &ZapSink{log: log}
}

func (s *ZapSink) WriteEvent(ev UsageEvent) {
	fields := []zap.Field{
		zap.String("rid", ev.RID),
		zap.String("tenant", ev.Tenant),
		zap.String("src", ev.Src),
		zap.String("stack", ev.Stack),
		zap.Int64("duration_ms", ev.DurationMS),
		zap.String("status", ev.Status),
		zap.Int("bytes_in", ev.BytesIn),
		zap.Int("bytes_out", ev.BytesOut),
	}
	// Only computes carry memory; keep request-level usage lines unchanged.
	if ev.MemBytes > 0 {
		fields = append(fields, zap.Int("mem_bytes", ev.MemBytes))
	}
	// Fuel is conditional too: empty/unrouted requests do no accounted
	// work and have nothing to meter. Single-tenant deployments see
	// this field for any real request; tenant-aware deployments
	// aggregate it for billing or quota enforcement.
	if ev.Fuel > 0 {
		fields = append(fields, zap.Int64("fuel", ev.Fuel))
	}
	// Admission denials: tag the line so log-based billing/analytics can
	// exclude rejected traffic. billable is emitted only when false — the
	// unstated default for a normal request is billable=true.
	if ev.AdmissionDenied {
		fields = append(fields, zap.Bool("admission_denied", true))
		if ev.AdmissionReason != "" {
			fields = append(fields, zap.String("admission_reason", ev.AdmissionReason))
		}
	}
	if !ev.Billable {
		fields = append(fields, zap.Bool("billable", false))
	}
	// The "_sys" tenant is chassis infrastructure (the _sys/boot
	// pipeline: health probe, unrouted 404s, routing) — not a customer
	// tenant, so it's never billable/metered usage. Demote it to Debug
	// to keep prod logs to real per-tenant traffic; still emitted at
	// --log-level=debug, and genuine unrouted requests are separately
	// visible via the "ingress reject (no_route)" line.
	if ev.Tenant == "_sys" {
		s.log.Debug("usage", fields...)
		return
	}
	s.log.Info("usage", fields...)
}

func (s *ZapSink) Close(context.Context) error { return nil }
