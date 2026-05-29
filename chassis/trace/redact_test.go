package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// --- ApplyHints unit tests -----------------------------------------

func TestApplyHints_Redact(t *testing.T) {
	in := []byte(`{"user":{"email":"a@b.com","name":"alice"},"other":42}`)
	out := ApplyHints(in, Hints{Redact: []string{"user.email"}})
	if gjson.GetBytes(out, "user.email").String() != "[REDACTED]" {
		t.Fatalf("expected [REDACTED] sentinel; got %s", out)
	}
	if gjson.GetBytes(out, "user.name").String() != "alice" {
		t.Fatalf("unrelated key was mangled: %s", out)
	}
	if gjson.GetBytes(out, "other").Int() != 42 {
		t.Fatalf("unrelated key was mangled: %s", out)
	}
}

func TestApplyHints_Omit(t *testing.T) {
	in := []byte(`{"user":{"email":"a@b.com","name":"alice"},"other":42}`)
	out := ApplyHints(in, Hints{Omit: []string{"user.email"}})
	if gjson.GetBytes(out, "user.email").Exists() {
		t.Fatalf("expected user.email to vanish; got %s", out)
	}
	if gjson.GetBytes(out, "user.name").String() != "alice" {
		t.Fatalf("unrelated key was mangled: %s", out)
	}
}

func TestApplyHints_OmitWinsOverRedact(t *testing.T) {
	in := []byte(`{"user":{"email":"a@b.com"}}`)
	out := ApplyHints(in, Hints{
		Omit:   []string{"user.email"},
		Redact: []string{"user.email"},
	})
	if gjson.GetBytes(out, "user.email").Exists() {
		t.Fatalf("omit should have won and deleted the field; got %s", out)
	}
}

func TestApplyHints_MissingPathIsNoop(t *testing.T) {
	in := []byte(`{"user":{"name":"alice"}}`)
	out := ApplyHints(in, Hints{
		Redact: []string{"user.email", "user.ssn"},
		Omit:   []string{"never.there"},
	})
	if !bytes.Equal(in, out) {
		t.Fatalf("expected byte-equal pass-through on no-match; got %s vs %s", in, out)
	}
}

func TestApplyHints_MultiplePaths(t *testing.T) {
	in := []byte(`{"a":1,"b":2,"c":3,"d":4}`)
	out := ApplyHints(in, Hints{
		Redact: []string{"a", "c"},
		Omit:   []string{"d"},
	})
	if gjson.GetBytes(out, "a").String() != "[REDACTED]" {
		t.Fatalf("a not redacted: %s", out)
	}
	if gjson.GetBytes(out, "b").Int() != 2 {
		t.Fatalf("b wrongly mangled: %s", out)
	}
	if gjson.GetBytes(out, "c").String() != "[REDACTED]" {
		t.Fatalf("c not redacted: %s", out)
	}
	if gjson.GetBytes(out, "d").Exists() {
		t.Fatalf("d not omitted: %s", out)
	}
}

func TestApplyHints_EmptyHintsIsPassthrough(t *testing.T) {
	in := []byte(`{"x":1}`)
	out := ApplyHints(in, Hints{})
	if !bytes.Equal(in, out) {
		t.Fatalf("expected byte-equal pass-through; got %s", out)
	}
}

func TestApplyHints_Idempotent(t *testing.T) {
	in := []byte(`{"x":"secret"}`)
	once := ApplyHints(in, Hints{Redact: []string{"x"}})
	twice := ApplyHints(once, Hints{Redact: []string{"x"}})
	if !bytes.Equal(once, twice) {
		t.Fatalf("expected idempotent redact: %s vs %s", once, twice)
	}
}

// --- RedactingSink integration tests ------------------------------

// captureSink records exactly what bytes the inner sink saw, in
// order. Each per-request slot keeps Payload, every Input/Output it
// received via Step, and the final End payload.
type captureSink struct {
	mu       sync.Mutex
	requests map[string]*captureReq
}

type captureReq struct {
	info   RequestInfo
	steps  []StepInfo
	events []TimelineEvent
	status string
	final  []byte
}

func newCaptureSink() *captureSink {
	return &captureSink{requests: map[string]*captureReq{}}
}

func (s *captureSink) Begin(info RequestInfo) RequestTracer {
	s.mu.Lock()
	defer s.mu.Unlock()
	infoCopy := info
	if info.Payload != nil {
		infoCopy.Payload = append([]byte(nil), info.Payload...)
	}
	req := &captureReq{info: infoCopy}
	s.requests[info.RID] = req
	return &captureTracer{sink: s, rid: info.RID}
}

func (s *captureSink) Close(context.Context) error { return nil }

func (s *captureSink) get(rid string) *captureReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[rid]
}

type captureTracer struct {
	sink *captureSink
	rid  string
}

func (t *captureTracer) Step(info StepInfo) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	r := t.sink.requests[t.rid]
	if r == nil {
		return
	}
	c := info
	if info.Input != nil {
		c.Input = append([]byte(nil), info.Input...)
	}
	if info.Output != nil {
		c.Output = append([]byte(nil), info.Output...)
	}
	r.steps = append(r.steps, c)
}

func (t *captureTracer) Event(ev TimelineEvent) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	r := t.sink.requests[t.rid]
	if r == nil {
		return
	}
	r.events = append(r.events, ev)
}

func (t *captureTracer) End(status string, final []byte) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	r := t.sink.requests[t.rid]
	if r == nil {
		return
	}
	r.status = status
	if final != nil {
		r.final = append([]byte(nil), final...)
	}
}

// fixedLookup is a HintLookup keyed by tenant + ":" + stack.
func fixedLookup(m map[string]Hints) HintLookup {
	return func(tenant, stack string) Hints {
		return m[tenant+":"+stack]
	}
}

func TestRedactingSink_NilLookupReturnsInnerUnchanged(t *testing.T) {
	inner := newCaptureSink()
	got := NewRedactingSink(inner, nil)
	// NewRedactingSink returns inner unchanged when lookup is nil —
	// no wrapper, no allocation.
	if got != Sink(inner) {
		t.Fatalf("expected nil lookup to return inner unchanged; got %T", got)
	}
}

func TestRedactingSink_BeginPayloadRedacted(t *testing.T) {
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		"acme:acme/support": {Redact: []string{"user.email"}},
	})
	wrapped := NewRedactingSink(inner, lookup)

	rt := wrapped.Begin(RequestInfo{
		RID:    "r1",
		Tenant: "acme",
		Stack:  "acme/support",
		Payload: []byte(`{"user":{"email":"a@b.com"}}`),
	})
	rt.End("ok", []byte(`{"user":{"email":"a@b.com"}}`))

	r := inner.get("r1")
	if r == nil {
		t.Fatal("inner never saw the request")
	}
	if got := gjson.GetBytes(r.info.Payload, "user.email").String(); got != "[REDACTED]" {
		t.Fatalf("Begin payload not redacted: %s", r.info.Payload)
	}
	if got := gjson.GetBytes(r.final, "user.email").String(); got != "[REDACTED]" {
		t.Fatalf("End final not redacted: %s", r.final)
	}
}

func TestRedactingSink_StepInputOutputRedacted(t *testing.T) {
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		"acme:acme/support": {
			Redact: []string{"req.token"},
			Omit:   []string{"req.attachments"},
		},
	})
	wrapped := NewRedactingSink(inner, lookup)

	rt := wrapped.Begin(RequestInfo{
		RID:    "r2",
		Tenant: "acme",
		Stack:  "acme/support",
		Payload: []byte(`{"hi":"there"}`),
	})
	rt.Step(StepInfo{
		Stack: "acme/support",
		Scope: 100,
		Name:  "classify",
		Input: []byte(`{"req":{"token":"sek","attachments":["big"]}}`),
		Output: []byte(`{"req":{"token":"sek2","other":"keep"}}`),
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	rt.End("ok", []byte(`{}`))

	r := inner.get("r2")
	if r == nil || len(r.steps) != 1 {
		t.Fatal("inner never saw the step")
	}
	step := r.steps[0]
	if got := gjson.GetBytes(step.Input, "req.token").String(); got != "[REDACTED]" {
		t.Fatalf("step Input token not redacted: %s", step.Input)
	}
	if gjson.GetBytes(step.Input, "req.attachments").Exists() {
		t.Fatalf("step Input attachments not omitted: %s", step.Input)
	}
	if got := gjson.GetBytes(step.Output, "req.token").String(); got != "[REDACTED]" {
		t.Fatalf("step Output token not redacted: %s", step.Output)
	}
	if got := gjson.GetBytes(step.Output, "req.other").String(); got != "keep" {
		t.Fatalf("unrelated Output field mangled: %s", step.Output)
	}
}

func TestRedactingSink_CrossTenantIsolation(t *testing.T) {
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		// Only acme/support declares the hint. beta/support inherits nothing.
		"acme:acme/support": {Redact: []string{"user.email"}},
	})
	wrapped := NewRedactingSink(inner, lookup)

	// Tenant beta — same stack name, different tenant. Should be untouched.
	bt := wrapped.Begin(RequestInfo{
		RID:    "rB",
		Tenant: "beta",
		Stack:  "beta/support",
		Payload: []byte(`{"user":{"email":"b@beta.com"}}`),
	})
	bt.End("ok", []byte(`{"user":{"email":"b@beta.com"}}`))

	rB := inner.get("rB")
	if got := gjson.GetBytes(rB.info.Payload, "user.email").String(); got != "b@beta.com" {
		t.Fatalf("tenant isolation broken: %s", rB.info.Payload)
	}
	if got := gjson.GetBytes(rB.final, "user.email").String(); got != "b@beta.com" {
		t.Fatalf("tenant isolation broken on End: %s", rB.final)
	}
}

func TestRedactingSink_CrossStackIsolation(t *testing.T) {
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		// Only acme/support declares the hint. acme/billing (same tenant,
		// different stack) inherits nothing.
		"acme:acme/support": {Redact: []string{"user.email"}},
	})
	wrapped := NewRedactingSink(inner, lookup)

	bt := wrapped.Begin(RequestInfo{
		RID:    "rBill",
		Tenant: "acme",
		Stack:  "acme/billing",
		Payload: []byte(`{"user":{"email":"a@acme.com"}}`),
	})
	bt.End("ok", []byte(`{"user":{"email":"a@acme.com"}}`))

	rB := inner.get("rBill")
	if got := gjson.GetBytes(rB.info.Payload, "user.email").String(); got != "a@acme.com" {
		t.Fatalf("cross-stack isolation broken: %s", rB.info.Payload)
	}
}

func TestRedactingSink_StackUnionAfterJump(t *testing.T) {
	// A request enters acme/support (which declares Omit:attachments)
	// then jumps into acme/billing (which declares Redact:user.email).
	// Final out.json should see BOTH stacks' hints applied — that's
	// the union semantic.
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		"acme:acme/support": {Omit: []string{"attachments"}},
		"acme:acme/billing": {Redact: []string{"user.email"}},
	})
	wrapped := NewRedactingSink(inner, lookup)

	rt := wrapped.Begin(RequestInfo{
		RID:    "rJ",
		Tenant: "acme",
		Stack:  "acme/support", // initial entry
		Payload: []byte(`{"attachments":["a"],"user":{"email":"x@y"}}`),
	})
	// Step records that we entered acme/billing.
	rt.Step(StepInfo{
		Stack: "acme/billing",
		Scope: 100,
		Name:  "charge",
		Input: []byte(`{"attachments":["a"],"user":{"email":"x@y"}}`),
		Output: []byte(`{"attachments":["a"],"user":{"email":"x@y"}}`),
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	})
	rt.End("ok", []byte(`{"attachments":["a"],"user":{"email":"x@y"}}`))

	r := inner.get("rJ")
	if gjson.GetBytes(r.final, "attachments").Exists() {
		t.Fatalf("union: attachments should be omitted on final: %s", r.final)
	}
	if got := gjson.GetBytes(r.final, "user.email").String(); got != "[REDACTED]" {
		t.Fatalf("union: user.email should be redacted on final: %s", r.final)
	}
	// Step's Input/Output should also see the union (Begin's stack +
	// Step's own stack are both registered by the time Step fires).
	step := r.steps[0]
	if gjson.GetBytes(step.Input, "attachments").Exists() {
		t.Fatalf("union: step Input attachments should be omitted: %s", step.Input)
	}
	if got := gjson.GetBytes(step.Output, "user.email").String(); got != "[REDACTED]" {
		t.Fatalf("union: step Output user.email should be redacted: %s", step.Output)
	}
}

func TestRedactingSink_EmptyStackSkipped(t *testing.T) {
	// Begin with no Stack pin (boot/% fallback). No hint should fire
	// even if the lookup has a key for the empty stack.
	inner := newCaptureSink()
	lookup := fixedLookup(map[string]Hints{
		"acme:": {Redact: []string{"x"}},
	})
	wrapped := NewRedactingSink(inner, lookup)
	rt := wrapped.Begin(RequestInfo{
		RID:    "rE",
		Tenant: "acme",
		Stack:  "",
		Payload: []byte(`{"x":"y"}`),
	})
	rt.End("ok", []byte(`{"x":"y"}`))
	r := inner.get("rE")
	if got := gjson.GetBytes(r.info.Payload, "x").String(); got != "y" {
		t.Fatalf("empty-stack should not redact: %s", r.info.Payload)
	}
}

// --- Hints helpers ------------------------------------------------

func TestHints_Empty(t *testing.T) {
	if !(Hints{}).Empty() {
		t.Fatal("zero Hints should be empty")
	}
	if (Hints{Redact: []string{"x"}}).Empty() {
		t.Fatal("Hints with redact should be non-empty")
	}
	if (Hints{Omit: []string{"x"}}).Empty() {
		t.Fatal("Hints with omit should be non-empty")
	}
}

// sortHints sorts both lists in place so tests don't depend on
// iteration order through maps inside ApplyHints / union.
func sortHints(h Hints) Hints {
	sort.Strings(h.Redact)
	sort.Strings(h.Omit)
	return h
}

func TestApplyHints_PreservesValidJSON(t *testing.T) {
	in := []byte(`{"deep":{"nest":{"v":"secret"}}}`)
	out := ApplyHints(in, Hints{Redact: []string{"deep.nest.v"}})
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out)
	}
	want := map[string]any{"deep": map[string]any{"nest": map[string]any{"v": "[REDACTED]"}}}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("structure mismatch: got %v want %v", v, want)
	}
}
