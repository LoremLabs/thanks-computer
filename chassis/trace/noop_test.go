package trace

import (
	"context"
	"testing"
	"time"
)

// TestNoopSinkIsAZeroValue exercises every method of NoopSink and
// NoopTracer to confirm they do nothing observable. Locks in that
// production (mode=off) chassis pay zero cost for tracing.
func TestNoopSinkIsAZeroValue(t *testing.T) {
	var s NoopSink
	tr := s.Begin(RequestInfo{
		RID:       "test-rid",
		Src:       "http",
		StartedAt: time.Now(),
		Payload:   []byte(`{"hello":"world"}`),
	})
	if tr == nil {
		t.Fatal("NoopSink.Begin returned nil tracer")
	}

	// All methods must accept arbitrary inputs and never panic.
	tr.Step(StepInfo{
		Stack: "s", Scope: 100, Name: "n",
		Input:      []byte("in"),
		Output:     []byte("out"),
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
		Status:     "ok",
	})
	tr.Event(TimelineEvent{Ts: time.Now(), Event: "ev", Fields: map[string]any{"k": "v"}})
	tr.End("ok", []byte(`{}`))
}

// TestFromContextReturnsNoopWhenAbsent locks in that FromContext is
// safe to call from anywhere — callers don't need nil checks.
func TestFromContextReturnsNoopWhenAbsent(t *testing.T) {
	tr := FromContext(context.Background())
	if tr == nil {
		t.Fatal("FromContext returned nil; expected a NoopTracer")
	}
	// Must not panic.
	tr.Step(StepInfo{})
	tr.Event(TimelineEvent{})
	tr.End("ok", nil)
}

func TestParseModeNormalizes(t *testing.T) {
	cases := map[string]Mode{
		"off":          ModeOff,
		"summary":      ModeSummary,
		"full":         ModeFull,
		"":             ModeOff,
		"unknown":      ModeOff, // typo falls back to off — never silently enable
		"FULL":         ModeOff, // case-sensitive on purpose: match the YAML literal
	}
	for in, want := range cases {
		if got := ParseMode(in); got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
}
