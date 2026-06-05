package trace

import "testing"

type usageCapture struct{ events []TimelineEvent }

func (c *usageCapture) Step(StepInfo)              {}
func (c *usageCapture) Event(ev TimelineEvent)     { c.events = append(c.events, ev) }
func (c *usageCapture) End(string, string, []byte) {}

// TestEmitUsage: the shared helper emits exactly one `request.usage` event
// carrying the fuel/bytes_out/tenant the readers lift onto the trace. This is
// the single definition every convergence point (main + 3 resume paths) uses.
func TestEmitUsage(t *testing.T) {
	c := &usageCapture{}
	EmitUsage(c, 105, 42, "prod-mankins")

	if len(c.events) != 1 {
		t.Fatalf("emitted %d events, want 1", len(c.events))
	}
	ev := c.events[0]
	if ev.Event != "request.usage" {
		t.Errorf("event = %q, want request.usage", ev.Event)
	}
	if ev.Fields["fuel"] != int64(105) {
		t.Errorf("fuel = %v (%T), want int64 105", ev.Fields["fuel"], ev.Fields["fuel"])
	}
	if ev.Fields["bytes_out"] != 42 {
		t.Errorf("bytes_out = %v, want 42", ev.Fields["bytes_out"])
	}
	if ev.Fields["tenant"] != "prod-mankins" {
		t.Errorf("tenant = %v, want prod-mankins", ev.Fields["tenant"])
	}
}
