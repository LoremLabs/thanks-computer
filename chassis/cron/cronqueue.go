// Package cron is the cron-dispatch seam. It defines Job (one unit of
// scheduled work) and Queue (the pluggable transport between the
// scheduler that produces ticks and the worker that dispatches them),
// plus a Register/Open registry mirroring chassis/feed.
//
// The built-in "local" backend (chassis/cron/local) is an in-process
// channel + worker pool: single-node, at-most-once, no broker. An
// out-of-tree backend (e.g. "nats") self-registers the same way feed
// backends do — init() + blank import — with zero changes to this
// package or the cron personality.
package cron

import (
	"context"
	"fmt"
)

// Job is one unit of scheduled work: a fully-built cron envelope payload
// plus the routing identity a backend needs. The scheduler computes the
// wall-clock fields and the bucket ONCE per tick, builds the payload
// JSON, and enqueues one Job per target. Carrying the already-built
// Payload (not raw time fields) keeps the seam transport-dumb: a backend
// relays bytes and never recomputes time — so a job that waits out its
// spread offset (or a broker hop) still reports the instant the tick
// fired, not the instant it ran.
type Job struct {
	// Tenant is the subscriber slug, or "" for the legacy system job.
	Tenant string
	// Job is the cron job name: "default" (legacy system tick) or
	// "_cron" (per-tenant fan-out). Stamped into Payload already;
	// duplicated here so a backend can key/dedup without parsing JSON.
	Job string
	// Bucket is the wall-clock period boundary this job belongs to,
	// e.g. "2026-06-02T15:24Z". Stable dedup key for an at-least-once
	// backend; the local backend ignores it (single delivery).
	Bucket string
	// MaxTime is the per-dispatch timeout budget in seconds (CronPeriod).
	MaxTime int
	// Payload is the complete cron envelope JSON (already carries
	// _txc.src=cron, _ts, _txc.cron.*, tenant, job). The worker pushes
	// this onto the bus after stamping a fresh _txc.rid; it does NOT
	// carry a rid (one is minted per delivery).
	Payload string
}

// Queue is the pluggable transport between scheduler (Enqueue side) and
// worker (Work side). A single process owns one Queue: the scheduler
// enqueues, the worker drains. For the local backend these are the same
// process and the job never leaves memory; for a broker backend Enqueue
// publishes and Work subscribes.
//
// Delivery contract (transport-neutral): Work MUST ensure that, for a
// given queued job, fn is invoked by at most one worker. fn errors are
// logged by the caller's closure and never requeued. An implementation
// that intentionally provides at-most-once / drop-on-crash semantics MAY
// acknowledge or claim a job before invoking fn (the "nats" backend
// does) — the seam itself prescribes no broker mechanics.
type Queue interface {
	// Enqueue submits one job. May block when the backend applies
	// backpressure (e.g. a full in-process buffer); honors ctx so a
	// shutting-down scheduler doesn't block forever.
	Enqueue(ctx context.Context, job Job) error

	// Work runs the consumer loop until ctx is cancelled, invoking fn
	// for each claimed job under the backend's concurrency policy. fn is
	// the dispatch closure supplied by the worker (build envelope, push
	// bus, drain response). Work returns when ctx is done and all
	// in-flight fn calls have drained.
	Work(ctx context.Context, fn func(context.Context, Job) error) error

	// Name is the backend identity, for boot logs (mirrors feed.Source).
	Name() string
}

// Config carries backend-selecting options resolved from chassis config.
// Additional backends may read fields they need; adding fields here does
// not affect existing backends or callers (same posture as
// feed.SourceConfig).
type Config struct {
	// MaxInflight bounds concurrent dispatches (--cron-max-inflight).
	MaxInflight int
	// Period is CronPeriod in seconds; backends may use it to size
	// buffers or a staleness window.
	Period int
}

// Constructor builds a Queue from resolved config.
type Constructor func(Config) (Queue, error)

// registry maps backend name → constructor. The "local" backend is built
// in; the map is the extension seam — an additional backend in a separate
// package registers itself the same way, with no change to the runner or
// this package.
var registry = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named backend. Unknown name is a startup error
// listing what is available.
func Open(name string, cfg Config) (Queue, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("cron: unknown queue %q (available: %v)", name, avail)
	}
	return c(cfg)
}
