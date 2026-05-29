package metrics

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// TestNewSmoke covers the no-export case: with no OTEL_* env vars set,
// New() must still return a Metrics with usable Tracer + Meter + every
// pre-built instrument, and Shutdown must not error.
func TestNewSmoke(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	conf := config.Config{Environment: "test", Fqdn: "test.local"}
	m := New(context.Background(), conf, zap.NewNop())

	if m.Tracer == nil {
		t.Fatal("Tracer is nil")
	}
	if m.Meter == nil {
		t.Fatal("Meter is nil")
	}
	if m.EventsReceived == nil {
		t.Fatal("EventsReceived counter is nil")
	}
	if m.EventsResponseTimeMs == nil {
		t.Fatal("EventsResponseTimeMs histogram is nil")
	}
	if m.OpTotal == nil {
		t.Fatal("OpTotal counter is nil")
	}
	if m.OpDurationMs == nil {
		t.Fatal("OpDurationMs histogram is nil")
	}

	// Spans and instruments should accept calls without panicking even when
	// no exporter is wired.
	ctx, span := m.Tracer.Start(context.Background(), "smoke")
	span.SetAttributes(attribute.String("k", "v"))
	span.End()

	m.EventsReceived.Add(ctx, 1)
	m.EventsResponseTimeMs.Record(ctx, 42)
	m.OpTotal.Add(ctx, 1)
	m.OpDurationMs.Record(ctx, 7)

	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}
}

// TestRecordEvent drives the Metrics.RecordEvent helper that server.go
// calls per envelope, and asserts both the events.received counter and
// the events.response_time histogram record under the right
// AttrEventsSource attribute.
func TestRecordEvent(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	conf := config.Config{Environment: "test", Fqdn: "test.local"}
	m := New(context.Background(), conf, zap.NewNop())

	ctx := context.Background()
	m.RecordEvent(ctx, "web", 12)
	m.RecordEvent(ctx, "web", 34)
	m.RecordEvent(ctx, "tcp", 99)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	counts := map[string]int64{}
	histSums := map[string]int64{}
	histCounts := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mm := range sm.Metrics {
			switch d := mm.Data.(type) {
			case metricdata.Sum[int64]:
				if mm.Name == "chassis.events.received" {
					for _, dp := range d.DataPoints {
						src, _ := dp.Attributes.Value(attribute.Key(AttrEventsSource))
						counts[src.AsString()] = dp.Value
					}
				}
			case metricdata.Histogram[int64]:
				if mm.Name == "chassis.events.response_time" {
					for _, dp := range d.DataPoints {
						src, _ := dp.Attributes.Value(attribute.Key(AttrEventsSource))
						histSums[src.AsString()] = dp.Sum
						histCounts[src.AsString()] = dp.Count
					}
				}
			}
		}
	}

	if counts["web"] != 2 {
		t.Errorf("counts[web] = %d, want 2", counts["web"])
	}
	if counts["tcp"] != 1 {
		t.Errorf("counts[tcp] = %d, want 1", counts["tcp"])
	}
	if histSums["web"] != 46 || histCounts["web"] != 2 {
		t.Errorf("response_time[web] sum=%d count=%d, want sum=46 count=2", histSums["web"], histCounts["web"])
	}
	if histSums["tcp"] != 99 || histCounts["tcp"] != 1 {
		t.Errorf("response_time[tcp] sum=%d count=%d, want sum=99 count=1", histSums["tcp"], histCounts["tcp"])
	}
}

// TestEventsReceivedRecording uses a ManualReader to verify the
// EventsReceived counter actually records with the AttrEventsSource
// attribute the chassis attaches at the call site. This guards against
// silent regressions where the wrong instrument or attribute name is wired.
func TestEventsReceivedRecording(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	meter := otel.Meter("test")
	counter, err := meter.Int64Counter("chassis.events.received")
	if err != nil {
		t.Fatalf("counter init: %v", err)
	}

	ctx := context.Background()
	attrs := metric.WithAttributes(attribute.String(AttrEventsSource, "web"))
	counter.Add(ctx, 1, attrs)
	counter.Add(ctx, 1, attrs)
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String(AttrEventsSource, "tcp")))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "chassis.events.received" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("expected Sum[int64], got %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				src, _ := dp.Attributes.Value(attribute.Key(AttrEventsSource))
				got[src.AsString()] = dp.Value
			}
		}
	}

	if got["web"] != 2 {
		t.Errorf("web counter = %d, want 2", got["web"])
	}
	if got["tcp"] != 1 {
		t.Errorf("tcp counter = %d, want 1", got["tcp"])
	}
}

// TestRecordSecretMaterialize verifies the per-(tenant,secret)
// counter increments correctly via the public RecordSecretMaterialize
// API and that the labels (tenant slug + secret name) are recorded.
// Operationally this is the signal operators consume to spot a
// runaway op (10k+/sec materialize on a single secret).
func TestRecordSecretMaterialize(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	meter := otel.Meter("test")
	counter, err := meter.Int64Counter("chassis.secret.materialize")
	if err != nil {
		t.Fatalf("counter init: %v", err)
	}
	m := &Metrics{SecretMaterialized: counter}

	ctx := context.Background()
	// acme/STRIPE: 3 references (e.g. one op fires 3 times).
	m.RecordSecretMaterialize(ctx, "acme", "STRIPE_API_KEY")
	m.RecordSecretMaterialize(ctx, "acme", "STRIPE_API_KEY")
	m.RecordSecretMaterialize(ctx, "acme", "STRIPE_API_KEY")
	// acme/SLACK: 1 reference.
	m.RecordSecretMaterialize(ctx, "acme", "SLACK_WEBHOOK")
	// other/STRIPE: 1 reference (different tenant, same name —
	// MUST be a separate counter slot, not folded with acme/STRIPE).
	m.RecordSecretMaterialize(ctx, "other", "STRIPE_API_KEY")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	type key struct{ tenant, name string }
	got := map[key]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "chassis.secret.materialize" {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("expected Sum[int64], got %T", mt.Data)
			}
			for _, dp := range sum.DataPoints {
				tenant, _ := dp.Attributes.Value(attribute.Key(AttrTenantSlug))
				name, _ := dp.Attributes.Value(attribute.Key(AttrSecretName))
				got[key{tenant.AsString(), name.AsString()}] = dp.Value
			}
		}
	}

	if got[key{"acme", "STRIPE_API_KEY"}] != 3 {
		t.Errorf("acme/STRIPE_API_KEY = %d, want 3", got[key{"acme", "STRIPE_API_KEY"}])
	}
	if got[key{"acme", "SLACK_WEBHOOK"}] != 1 {
		t.Errorf("acme/SLACK_WEBHOOK = %d, want 1", got[key{"acme", "SLACK_WEBHOOK"}])
	}
	if got[key{"other", "STRIPE_API_KEY"}] != 1 {
		t.Errorf("other/STRIPE_API_KEY = %d, want 1 (must NOT fold with acme/STRIPE_API_KEY)", got[key{"other", "STRIPE_API_KEY"}])
	}
}

// TestRecordSecretMaterializeNilSafe documents the nil-safe contract:
// a Metrics built without the secret counter (e.g. some unit tests
// that pre-date the field) must NOT panic when the splice records.
func TestRecordSecretMaterializeNilSafe(t *testing.T) {
	// Nil Metrics receiver.
	var m *Metrics
	m.RecordSecretMaterialize(context.Background(), "acme", "K")

	// Non-nil Metrics with nil counter.
	m2 := &Metrics{}
	m2.RecordSecretMaterialize(context.Background(), "acme", "K")
}
