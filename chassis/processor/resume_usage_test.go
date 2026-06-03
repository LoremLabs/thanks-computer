package processor

import (
	"context"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// chanUsage is a race-safe usage.Sink for cross-goroutine assertions (the
// resume billing fires from a detached goroutine; the suite runs with -race).
type chanUsage struct{ ch chan usage.UsageEvent }

func newChanUsage() *chanUsage                     { return &chanUsage{ch: make(chan usage.UsageEvent, 8)} }
func (c *chanUsage) WriteEvent(e usage.UsageEvent) { c.ch <- e }
func (c *chanUsage) Name() string                  { return "chan" }
func (c *chanUsage) Close(context.Context) error   { return nil }

// TestEmitResumeUsage: the per-segment delta + field mapping. Synchronous, so
// the simple stubUsage (compute_test.go) is fine here.
func TestEmitResumeUsage(t *testing.T) {
	pu, _ := newTestUnit(t)
	us := &stubUsage{}
	pu.Usage = us

	ss := continuation.StageSuspended{
		ScopeEnvelope: `{"_txc":{"fuel_used":100,"tenant":"prod-mankins","stack":"site"}}`,
	}
	final := []byte(`{"_txc":{"fuel_used":175}}`)
	pu.emitResumeUsage(ss, final, "run_x", "site/100", 5*time.Millisecond)

	if us.ev == nil {
		t.Fatal("no usage event emitted")
	}
	if us.ev.Fuel != 75 {
		t.Errorf("Fuel = %d, want 75 (175-100 delta)", us.ev.Fuel)
	}
	if us.ev.Tenant != "prod-mankins" {
		t.Errorf("Tenant = %q, want prod-mankins", us.ev.Tenant)
	}
	if us.ev.Stack != "site" {
		t.Errorf("Stack = %q, want site", us.ev.Stack)
	}
	if us.ev.Src != "continuation" {
		t.Errorf("Src = %q, want continuation", us.ev.Src)
	}
	if !us.ev.Billable {
		t.Error("Billable = false, want true")
	}
	if us.ev.BytesOut != len(final) {
		t.Errorf("BytesOut = %d, want %d", us.ev.BytesOut, len(final))
	}
	if us.ev.RID != continuation.ResumeTraceRID("run_x", "site/100") {
		t.Errorf("RID = %q, want the resume trace rid", us.ev.RID)
	}
}

// TestEmitResumeUsageNegativeGuard: exit < entry (shouldn't happen, fuel only
// accrues) clamps to 0 rather than billing a negative.
func TestEmitResumeUsageNegativeGuard(t *testing.T) {
	pu, _ := newTestUnit(t)
	us := &stubUsage{}
	pu.Usage = us
	pu.emitResumeUsage(continuation.StageSuspended{ScopeEnvelope: `{"_txc":{"fuel_used":100}}`},
		[]byte(`{"_txc":{"fuel_used":40}}`), "run_x", "s/0", time.Millisecond)
	if us.ev == nil || us.ev.Fuel != 0 {
		t.Errorf("Fuel = %+v, want 0 (clamped)", us.ev)
	}
}

// TestEmitResumeUsageNilSink: nil-safe when usage is disabled.
func TestEmitResumeUsageNilSink(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Usage = nil
	pu.emitResumeUsage(continuation.StageSuspended{ScopeEnvelope: "{}"}, []byte("{}"), "r", "s/0", 0) // must not panic
}
