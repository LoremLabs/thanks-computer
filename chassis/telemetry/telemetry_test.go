package telemetry

import (
	"context"
	"testing"
	"time"
)

// fakeExporter records Record calls.
type fakeExporter struct {
	calls  int
	tenant string
	events []MetricEvent
}

func (f *fakeExporter) Name() string { return "fake" }
func (f *fakeExporter) Record(_ context.Context, tenant string, events []MetricEvent) {
	f.calls++
	f.tenant = tenant
	f.events = events
}
func (f *fakeExporter) Close(context.Context) error { return nil }

func TestProcessorFastPaths(t *testing.T) {
	exp := &fakeExporter{}
	rec := newDropRecorder()
	p := NewProcessor(exp, nil, rec.fn())

	p.Process(context.Background(), nil, "driplit", "driplit/www", "http")
	p.Process(context.Background(), []byte(`{"data":1}`), "driplit", "driplit/www", "http")
	// All-invalid intents: validated away, exporter untouched.
	p.Process(context.Background(), envelopeWith(`[{"kind":"counter","value":1}]`), "driplit", "driplit/www", "http")

	if exp.calls != 0 {
		t.Errorf("exporter called on fast paths: %d", exp.calls)
	}
	if rec.counts[dropInvalidName] != 1 {
		t.Errorf("invalid intent not counted: %v", rec.counts)
	}

	// Nil processor + nil-exporter processor are safe.
	var nilP *Processor
	nilP.Process(context.Background(), envelopeWith(`[]`), "t", "s", "http")
	if err := nilP.Close(context.Background()); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	NewProcessor(nil, nil, nil).Process(context.Background(), envelopeWith(`[]`), "t", "s", "http")
}

func TestProcessorUnpinnedDrops(t *testing.T) {
	exp := &fakeExporter{}
	rec := newDropRecorder()
	p := NewProcessor(exp, nil, rec.fn())

	payload := envelopeWith(`[{"name":"a","kind":"counter","value":1},{"name":"b","kind":"counter","value":1}]`)
	p.Process(context.Background(), payload, "", "s", "http")

	if exp.calls != 0 {
		t.Fatalf("exporter must never run without a pinned tenant")
	}
	if rec.counts[dropUnpinned] != 2 {
		t.Errorf("unpinned drop count = %v, want 2", rec.counts)
	}
}

func TestProcessorHappyPath(t *testing.T) {
	exp := &fakeExporter{}
	p := NewProcessor(exp, nil, nil)

	payload := envelopeWith(`[{"name":"book.queued","kind":"counter","value":1}]`)
	p.Process(context.Background(), payload, "driplit", "driplit/www", "http")

	if exp.calls != 1 {
		t.Fatalf("Record calls = %d, want 1", exp.calls)
	}
	if exp.tenant != "driplit" {
		t.Errorf("tenant = %q (must be the pinned arg, not envelope _txc.tenant=%q)", exp.tenant, "spoofed")
	}
	if len(exp.events) != 1 || exp.events[0].Name != "book.queued" {
		t.Errorf("events = %+v", exp.events)
	}
}

func TestProcessorWarnRateLimit(t *testing.T) {
	p := NewProcessor(&fakeExporter{}, nil, nil)
	current := t0
	p.now = func() time.Time { return current }

	p.warn("t", "r", "msg") // fires; gate = t0
	current = t0.Add(30 * time.Second)
	p.warn("t", "r", "msg") // within warnInterval — suppressed, gate unchanged
	if got := p.lastWarn["t\x00r"]; !got.Equal(t0) {
		t.Errorf("suppressed warn must not refresh the gate; got %v", got)
	}
	current = t0.Add(90 * time.Second)
	p.warn("t", "r", "msg") // past the interval — fires, gate refreshes
	if got := p.lastWarn["t\x00r"]; !got.Equal(current) {
		t.Errorf("third warn should refresh the gate; got %v", got)
	}
	if len(p.lastWarn) != 1 {
		t.Errorf("gate map should hold one key, got %d", len(p.lastWarn))
	}
}
