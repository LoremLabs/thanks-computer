// Package trace records per-request artifacts (the inbound envelope,
// every op execution, the final response, plus a timeline of routing
// events) to durable storage. Developers browse the result to see
// exactly what happened during a request without rerunning it or
// turning on debug logging.
//
// The package is interface-first: the chassis depends only on `Sink`
// and `RequestTracer`. `NoopSink` (mode=off) is the prod default and
// a true zero-cost no-op. `FileSink` (mode=summary|full) writes the
// artifact tree under a configured directory. A future `SQLiteSink`
// can be added behind the same interface without touching the
// processor or inlets.
//
// File layout under `<trace_dir>/requests/<rid>/`:
//
//	in.json            inlet's initial envelope (input to the request)
//	out.json           chassis's final response (after all merges)
//	timeline.jsonl     line-per-event log of stages/steps/jumps
//	steps/
//	  NNNN-<name>/     one folder per fired op, prefixed by zero-
//	                   padded scope so the dir lex-sorts in scope order
//	    op.json        the rule's stored definition (txcl, exec, etc.)
//	    in.json        envelope handed to the handler
//	    out.json       handler's raw response
//	    meta.json      timing, sizes, status, transport
//
// `in.json`/`out.json` are paired at every level — request root and
// per-step — so a developer can `diff` request-level in vs out, or
// step-level in vs out, with the same naming.
//
// In `summary` mode the per-step `op.json`/`in.json`/`out.json` are
// omitted; `meta.json` and the timeline still record what ran and
// how long it took.
package trace

import (
	"context"
	"time"
)

// Mode controls how much detail Sinks write.
//
//	off      no writes — production default.
//	summary  request + timeline + per-step meta. No payload bytes.
//	full     everything, including handler in/out bodies per step.
type Mode string

const (
	ModeOff     Mode = "off"
	ModeSummary Mode = "summary"
	ModeFull    Mode = "full"
)

// ParseMode normalizes user-supplied strings. Anything unrecognized
// falls back to ModeOff so a typo in TXCO_TRACE_MODE doesn't silently
// enable tracing in production.
func ParseMode(s string) Mode {
	switch s {
	case "summary":
		return ModeSummary
	case "full":
		return ModeFull
	default:
		return ModeOff
	}
}

// RequestInfo is what the chassis hands the sink at request start.
// Payload is the raw envelope bytes that landed in the chassis after
// inlet construction and any ingress stamping.
//
// PayloadBytes is the ORIGINAL payload size before any truncation a
// wrapping sink may apply. Zero means "use len(Payload)" — direct
// callers (no wrapper) don't need to set it. AsyncSink sets it to
// the true size before capping `Payload` to BodyCapBytes so meta.json
// records what was sent, not just what we kept on disk.
type RequestInfo struct {
	RID          string
	Src          string
	Tenant       string
	Stack        string
	StartedAt    time.Time
	Payload      []byte
	PayloadBytes int
}

// StepInfo describes one op execution.
//
// Stack/Scope/Name uniquely identify the rule that fired (the same
// triple the chassis stamps as `_txc.op` on outbound envelopes).
// Operation is the literal EXEC operand (a URL, txco://, or stage
// jump). Transport classifies the dispatch path.
//
// Input is the envelope bytes the chassis posted to the handler;
// Output is the handler's raw response. Both are recorded only in
// ModeFull.
type StepInfo struct {
	Stack      string
	Scope      int
	Name       string
	Operation  string
	Transport  string
	Txcl       string
	Input      []byte
	Output     []byte
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string
	Error      string

	// InputBytes / OutputBytes are the ORIGINAL payload sizes before
	// any truncation a wrapping sink may apply. Zero means "use
	// len(Input)/len(Output)" — direct callers don't need to set
	// these. AsyncSink populates them before capping the slices so
	// meta.json records what the handler actually saw, not just what
	// we kept on disk.
	InputBytes  int
	OutputBytes int
}

// TimelineEvent is a single line in timeline.jsonl. Event values are
// stable strings; Fields is open-ended (the sink marshals as-is).
type TimelineEvent struct {
	Ts     time.Time
	Event  string
	Fields map[string]any
}

// Sink hands out a RequestTracer per inbound request. Implementations
// must be safe for concurrent calls to Begin.
//
// Close is called once on chassis shutdown. Synchronous sinks
// (NoopSink, FileSink) can return nil immediately. Buffered/async
// sinks should drain any in-flight work, respecting the ctx deadline
// — return ctx.Err() if the drain doesn't complete in time.
type Sink interface {
	Begin(info RequestInfo) RequestTracer
	Close(ctx context.Context) error
}

// RequestTracer is the per-request handle. Step / Event must be safe
// for concurrent calls (parallel ops at the same scope fire from
// different goroutines). End is called exactly once.
//
// reason carries a human-readable explanation of a non-ok status — the
// pipeline error (e.g. "canceled while running test-stack/50 mcp+https://…")
// — so a trace can answer "why is it an error?" at the request level. It
// is "" on success. The request-level Status field has always been wired;
// reason is its missing companion.
type RequestTracer interface {
	Step(info StepInfo)
	Event(ev TimelineEvent)
	End(status, reason string, finalPayload []byte)
}

// EmitUsage records the per-request usage primitives — fuel, response size,
// and the resolved tenant slug — as a `request.usage` timeline event. The
// trace readers (file + NATS) lift these onto the trace's Fuel / BytesOut /
// Tenant; the tenant is what admin tenant-scoping filters on. Called from
// every convergence point — the main request path (server.runWithTrace) and
// each resume path (web/continuation, processor/deferred, processor/
// continuable) — so the event name + field keys live in exactly one place.
//
// This is a TRACE artifact only: it is NOT the billing usage.UsageEvent and
// never reaches the usage Sink, so emitting it on additional paths does not
// affect metering.
func EmitUsage(t RequestTracer, fuel int64, bytesOut int, tenant string) {
	t.Event(TimelineEvent{
		Ts:    time.Now(),
		Event: "request.usage",
		Fields: map[string]any{
			"fuel":      fuel,
			"bytes_out": bytesOut,
			"tenant":    tenant,
		},
	})
}
