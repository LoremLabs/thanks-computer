package trace

import (
	"context"
	"strings"
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

// A hint starts when a request reaches the stack that declares it, and every
// request starts in `_sys`, so the inbound record and the boot steps are
// written before any hint of a tenant's applies. What must never reach a trace
// is therefore scrubbed here, from the first record, whatever the stacks say.
var (
	// sensitiveHeaders are matched in any case: a header's spelling depends on
	// who wrote it (the web head canonicalizes, a rule's response writes lower
	// case).
	sensitiveHeaders = map[string]bool{
		"authorization":       true,
		"proxy-authorization": true,
		"cookie":              true,
		"set-cookie":          true,
		"x-api-key":           true,
		"x-auth-token":        true,
	}
	headerRoots = []string{"_txc.web.req.headers", "_txc.web.res.headers", "_txc.llm.headers"}
	// builtinHints are the same rule for what sits at a fixed path.
	builtinHints = Hints{
		Redact: []string{"_txc.web.req.cookies"},
		Omit:   []string{"_txc.lmtp.msg.raw", "_txc.lmtp.msg.attachments", "_txc.lmtp.msg.html"},
	}
)

// Scrub removes what never reaches a trace. b must be whole JSON: a value cut
// short cannot be found (AsyncSink scrubs before it caps). It never mutates b.
func Scrub(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	var paths []string
	for _, root := range headerRoots {
		gjson.GetBytes(b, root).ForEach(func(k, _ gjson.Result) bool {
			if sensitiveHeaders[strings.ToLower(k.String())] {
				paths = append(paths, root+"."+gjson.Escape(k.String()))
			}
			return true
		})
	}
	return ApplyHints(ApplyHints(b, builtinHints), Hints{Redact: paths})
}

// droppedRecord stands in for a record cut short that a stack's hints cover: a
// hint cannot be applied to JSON that has lost its end, and keeping it would
// keep what the stack asked to hide.
var droppedRecord = []byte(`{"_trace":"dropped: cut to the size cap, so this stack's redact and omit hints could not be applied"}`)

// scrubWith is Scrub, then a stack's hints. cut says the record was capped;
// noScrub skips Scrub and leaves the hints in force.
func scrubWith(b []byte, h Hints, cut, noScrub bool) []byte {
	if cut && !h.Empty() && len(b) > 0 && !gjson.ValidBytes(b) {
		return droppedRecord
	}
	// A capped record was scrubbed whole before the cap (AsyncSink).
	if !cut && !noScrub {
		b = Scrub(b)
	}
	return ApplyHints(b, h)
}

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
// A nil lookup means no stack hints: Scrub still runs on every record.
type RedactingSink struct {
	inner   Sink
	lookup  HintLookup
	noScrub bool
}

// NewRedactingSink wraps inner with redaction. Every sink is wrapped, also
// with a nil lookup (no rule declared any redact/omit). Scrub runs on every
// record unless the operator asked for NewHintOnlySink.
func NewRedactingSink(inner Sink, lookup HintLookup) Sink {
	return &RedactingSink{inner: inner, lookup: lookup}
}

// NewHintOnlySink applies stacks' hints but not Scrub, so credentials and raw
// mail reach the trace. It exists for debugging auth and mail on a chassis whose
// traces hold nothing worth protecting: --trace-no-scrub. Pair it with
// AsyncOpts.NoScrub, or capped records are still scrubbed.
func NewHintOnlySink(inner Sink, lookup HintLookup) Sink {
	return &RedactingSink{inner: inner, lookup: lookup, noScrub: true}
}

// Begin captures the request's tenant and (initial) stack, applies hints to
// the inbound payload, then forwards to the inner sink.
func (s *RedactingSink) Begin(info RequestInfo) RequestTracer {
	rt := &redactingTracer{
		lookup:  s.lookup,
		noScrub: s.noScrub,
		tenant:  info.Tenant,
		slots:   map[slot]struct{}{},
	}
	if info.Stack != "" {
		rt.slots[slot{info.Tenant, info.Stack}] = struct{}{}
	}
	if info.RedactStack != "" {
		rt.slots[slot{info.Tenant, info.RedactStack}] = struct{}{}
	}
	info.Payload = scrubWith(info.Payload, rt.union(), info.PayloadBytes > len(info.Payload), s.noScrub)
	rt.inner = s.inner.Begin(info)
	return rt
}

// Close forwards to the inner sink.
func (s *RedactingSink) Close(ctx context.Context) error { return s.inner.Close(ctx) }

// redactingTracer wraps the inner RequestTracer and applies the running
// union of hints — every (tenant, stack) the request has visited — on every
// forwarded call.
//
// The tenant is the STEP's. Every request enters the boot pipeline pinned
// to `_sys` and is re-tenanted by routing, so the tenant Begin sees is
// `_sys` for nearly every request; keying a tenant stack's hints by it
// found nothing, and no tenant's `WITH redact` / `WITH omit` ever applied.
type redactingTracer struct {
	inner   RequestTracer
	lookup  HintLookup
	noScrub bool
	tenant  string // Begin's: the fallback for a step that carries none

	mu    sync.Mutex // guards slots (parallel ops at the same scope share one tracer)
	slots map[slot]struct{}
}

// slot is one (tenant, stack) a request has run in.
type slot struct{ tenant, stack string }

// union builds the deduplicated hint lists for every (tenant, stack) the
// request has visited so far. Omit wins on collision.
func (t *redactingTracer) union() Hints {
	if t.lookup == nil {
		return Hints{}
	}
	t.mu.Lock()
	snap := make([]slot, 0, len(t.slots))
	for sl := range t.slots {
		snap = append(snap, sl)
	}
	t.mu.Unlock()
	if len(snap) == 0 {
		return Hints{}
	}
	var out Hints
	seenR := map[string]struct{}{}
	seenO := map[string]struct{}{}
	for _, sl := range snap {
		h := t.lookup(sl.tenant, sl.stack)
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
		tenant := info.Tenant
		if tenant == "" {
			tenant = t.tenant
		}
		t.mu.Lock()
		t.slots[slot{tenant, info.Stack}] = struct{}{}
		t.mu.Unlock()
	}
	h := t.union()
	info.Input = scrubWith(info.Input, h, info.InputBytes > len(info.Input), t.noScrub)
	info.Output = scrubWith(info.Output, h, info.OutputBytes > len(info.Output), t.noScrub)
	t.inner.Step(info)
}

func (t *redactingTracer) Event(ev TimelineEvent) {
	t.inner.Event(ev)
}

func (t *redactingTracer) End(status, reason string, finalPayload []byte) {
	// End carries no original size: a capped final shows as invalid JSON.
	finalPayload = scrubWith(finalPayload, t.union(), len(finalPayload) > 0 && !gjson.ValidBytes(finalPayload), t.noScrub)
	t.inner.End(status, reason, finalPayload)
}
