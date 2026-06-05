package trace

import (
	"context"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Hints are the redact/omit path lists for one (tenant, stack) slot.
// Redact replaces matched path values with the sentinel
// "[REDACTED]"; Omit deletes the path entirely. Both lists are
// gjson dot-paths (exact match only; no wildcards in v1).
//
// Same path appearing in both lists is resolved at build time: Omit
// wins, the path is dropped from Redact. See chassis/server/redact.go
// for the build-side dedupe.
type Hints struct {
	Redact []string
	Omit   []string
}

// Empty reports whether either list has work to do.
func (h Hints) Empty() bool { return len(h.Redact) == 0 && len(h.Omit) == 0 }

// HintLookup answers "what hints apply to this (tenant, stack) write?"
// AsyncSink calls it on the worker thread, off the request hot path.
// nil lookup ⇒ no redaction ever happens (the zero-cost default).
//
// Stack may be the empty string when an envelope hasn't been routed
// (boot/% fallback, system paths); callers should treat that as
// "lookup with stack=”" — the registry returns empty hints unless an
// untenanted/unstacked rule explicitly declared something.
type HintLookup func(tenant, stack string) Hints

// ApplyHints applies omits-then-redacts to b. Order matters at the
// margin: applying omits first lets `_txc.lmtp.msg` vanish without
// wasting a sentinel write on `_txc.lmtp.msg.headers.authorization`.
// It also makes behavior deterministic when an author lists the same
// path in both kinds. Returns the (possibly mutated) byte slice; the
// underlying array may or may not be reused by sjson.
func ApplyHints(b []byte, h Hints) []byte {
	if len(b) == 0 || h.Empty() {
		return b
	}
	for _, p := range h.Omit {
		if !gjson.GetBytes(b, p).Exists() {
			continue
		}
		if out, err := sjson.DeleteBytes(b, p); err == nil {
			b = out
		}
	}
	for _, p := range h.Redact {
		if !gjson.GetBytes(b, p).Exists() {
			continue
		}
		if out, err := sjson.SetBytes(b, p, "[REDACTED]"); err == nil {
			b = out
		}
	}
	return b
}

// RedactingSink wraps another Sink and applies a per-(tenant, stack)
// HintLookup to every envelope-bearing event before forwarding to the
// inner sink. Composable above or below an AsyncSink:
//
//	AsyncSink(RedactingSink(FileSink))   ← redaction runs on the
//	                                       async worker, off the
//	                                       request hot path
//	RedactingSink(FileSink)              ← sync mode; redaction runs
//	                                       on the request goroutine
//	                                       just before disk write
//
// nil lookup ⇒ NewRedactingSink returns the inner sink unchanged.
// The hot path stays exactly as it was when no redaction is
// configured.
type RedactingSink struct {
	inner  Sink
	lookup HintLookup
}

// NewRedactingSink wraps inner with redaction. When lookup is nil
// (no rule declared any redact/omit), returns inner unchanged — no
// allocation, no wrapper.
func NewRedactingSink(inner Sink, lookup HintLookup) Sink {
	if lookup == nil {
		return inner
	}
	return &RedactingSink{inner: inner, lookup: lookup}
}

// Begin captures the request's tenant and (initial) stack, applies
// hints to the inbound payload, then forwards to the inner sink.
func (s *RedactingSink) Begin(info RequestInfo) RequestTracer {
	rt := &redactingTracer{
		lookup: s.lookup,
		tenant: info.Tenant,
		stacks: map[string]struct{}{},
	}
	if info.Stack != "" {
		rt.stacks[info.Stack] = struct{}{}
	}
	info.Payload = ApplyHints(info.Payload, rt.union())
	rt.inner = s.inner.Begin(info)
	return rt
}

// Close forwards to the inner sink.
func (s *RedactingSink) Close(ctx context.Context) error { return s.inner.Close(ctx) }

// redactingTracer wraps the inner RequestTracer and applies the
// running union of hints (Begin's stack + every Step's stack) on
// every forwarded call.
type redactingTracer struct {
	inner  RequestTracer
	lookup HintLookup
	tenant string

	mu     sync.Mutex // guards stacks (parallel ops at the same scope share one tracer)
	stacks map[string]struct{}
}

// union builds the deduplicated hint lists for every (tenant, stack)
// the request has visited so far. Omit wins on collision.
func (t *redactingTracer) union() Hints {
	if t.lookup == nil {
		return Hints{}
	}
	t.mu.Lock()
	stackSnap := make([]string, 0, len(t.stacks))
	for st := range t.stacks {
		stackSnap = append(stackSnap, st)
	}
	t.mu.Unlock()
	if len(stackSnap) == 0 {
		return Hints{}
	}
	var out Hints
	seenR := map[string]struct{}{}
	seenO := map[string]struct{}{}
	for _, st := range stackSnap {
		h := t.lookup(t.tenant, st)
		for _, p := range h.Redact {
			if _, dup := seenR[p]; dup {
				continue
			}
			seenR[p] = struct{}{}
			out.Redact = append(out.Redact, p)
		}
		for _, p := range h.Omit {
			if _, dup := seenO[p]; dup {
				continue
			}
			seenO[p] = struct{}{}
			out.Omit = append(out.Omit, p)
		}
	}
	if len(out.Omit) > 0 && len(out.Redact) > 0 {
		filtered := out.Redact[:0]
		for _, p := range out.Redact {
			if _, drop := seenO[p]; drop {
				continue
			}
			filtered = append(filtered, p)
		}
		out.Redact = filtered
	}
	return out
}

func (t *redactingTracer) Step(info StepInfo) {
	if info.Stack != "" {
		t.mu.Lock()
		t.stacks[info.Stack] = struct{}{}
		t.mu.Unlock()
	}
	h := t.union()
	info.Input = ApplyHints(info.Input, h)
	info.Output = ApplyHints(info.Output, h)
	t.inner.Step(info)
}

func (t *redactingTracer) Event(ev TimelineEvent) {
	t.inner.Event(ev)
}

func (t *redactingTracer) End(status, reason string, finalPayload []byte) {
	finalPayload = ApplyHints(finalPayload, t.union())
	t.inner.End(status, reason, finalPayload)
}
