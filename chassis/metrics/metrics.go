// Package metrics wires OpenTelemetry tracing and metrics for the chassis.
//
// Configuration is via standard OTel environment variables:
//   - OTEL_EXPORTER_OTLP_ENDPOINT   (e.g. http://localhost:4318)
//   - OTEL_EXPORTER_OTLP_PROTOCOL   (http/protobuf | grpc)
//   - OTEL_SERVICE_NAME             (default: txco-chassis)
//   - OTEL_RESOURCE_ATTRIBUTES      (extra resource attributes)
//
// If no OTLP endpoint is set the SDK still installs a working tracer/meter
// provider; spans and metrics are recorded in-process but not exported.
package metrics

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// instrumentationName is the tracer / meter scope.
const instrumentationName = "github.com/loremlabs/thanks-computer/chassis"

// Attribute keys used across the chassis. These follow OTel semantic-convention
// style (lower-snake-case, dotted namespace) so they roll up cleanly in any
// OTLP backend.
const (
	AttrEventsSource = "txco.events.source"
	AttrOpName       = "txco.op.name"
	AttrTenantSlug   = "txco.tenant.slug"
	AttrSecretName   = "txco.secret.name"
)

// Metrics holds the chassis tracer, meter, and pre-built instruments.
type Metrics struct {
	Tracer trace.Tracer
	Meter  metric.Meter

	EventsReceived       metric.Int64Counter
	EventsResponseTimeMs metric.Int64Histogram
	OpTotal              metric.Int64Counter
	OpDurationMs         metric.Int64Histogram

	// SecretMaterialized increments once per (tenant_slug, secret_name)
	// reference the processor's splice resolves for an op (cache hit
	// or miss — both count as a reference). Lets operators detect a
	// runaway op that materializes the same secret thousands of times
	// per second, AND see the per-tenant access pattern for audit/
	// quota purposes. NEVER record the cleartext or the cleartext
	// length here — only the names.
	SecretMaterialized metric.Int64Counter

	shutdown []func(context.Context) error
}

// New initialises OTel providers and returns a Metrics with pre-built
// instruments. Call Metrics.Shutdown(ctx) before process exit to flush.
func New(ctx context.Context, conf config.Config, logger *zap.Logger) *Metrics {
	res := buildResource(conf)

	var shutdownFns []func(context.Context) error

	if otlpEnabled() {
		traceExp, err := otlptrace.New(ctx, otlptracehttp.NewClient())
		if err != nil {
			logger.Warn("OTel trace exporter init failed", zap.Error(err))
		} else {
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(traceExp),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(tp)
			shutdownFns = append(shutdownFns, tp.Shutdown)
		}

		metricExp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			logger.Warn("OTel metric exporter init failed", zap.Error(err))
		} else {
			mp := sdkmetric.NewMeterProvider(
				sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
				sdkmetric.WithResource(res),
			)
			otel.SetMeterProvider(mp)
			shutdownFns = append(shutdownFns, mp.Shutdown)
		}

		logger.Info("OTel enabled", zap.String("endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")))
	} else {
		// No OTLP env vars: leave the global providers alone. By default OTel
		// installs no-op providers, so calls are free-cost. Embedders or tests
		// can install their own providers before calling New() and we'll pick
		// up their tracer/meter via the otel.Tracer / otel.Meter calls below.
		_ = res // resource is unused in this branch
		logger.Info("OTel telemetry disabled (set OTEL_EXPORTER_OTLP_ENDPOINT to enable export)")
	}

	tracer := otel.Tracer(instrumentationName)
	meter := otel.Meter(instrumentationName)

	eventsReceived, err := meter.Int64Counter(
		"chassis.events.received",
		metric.WithDescription("Count of events received by the chassis"),
		metric.WithUnit("1"),
	)
	if err != nil {
		logger.Fatal("create events.received counter", zap.Error(err))
	}

	eventsResponseTimeMs, err := meter.Int64Histogram(
		"chassis.events.response_time",
		metric.WithDescription("Total event processing time"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		logger.Fatal("create events.response_time histogram", zap.Error(err))
	}

	opTotal, err := meter.Int64Counter(
		"chassis.op.count",
		metric.WithDescription("Count of operations executed"),
		metric.WithUnit("1"),
	)
	if err != nil {
		logger.Fatal("create op.count counter", zap.Error(err))
	}

	opDurationMs, err := meter.Int64Histogram(
		"chassis.op.duration",
		metric.WithDescription("Operation duration"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		logger.Fatal("create op.duration histogram", zap.Error(err))
	}

	secretMaterialized, err := meter.Int64Counter(
		"chassis.secret.materialize",
		metric.WithDescription("Count of secret references materialized for op execution (cache hits + misses)"),
		metric.WithUnit("1"),
	)
	if err != nil {
		logger.Fatal("create secret.materialize counter", zap.Error(err))
	}

	return &Metrics{
		Tracer:               tracer,
		Meter:                meter,
		EventsReceived:       eventsReceived,
		EventsResponseTimeMs: eventsResponseTimeMs,
		OpTotal:              opTotal,
		OpDurationMs:         opDurationMs,
		SecretMaterialized:   secretMaterialized,
		shutdown:             shutdownFns,
	}
}

// Shutdown flushes any pending telemetry. Call from main.go on signal.
func (m *Metrics) Shutdown(ctx context.Context) error {
	var firstErr error
	for _, fn := range m.shutdown {
		if err := fn(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RecordEvent increments the events.received counter and records the
// events.response_time histogram, both tagged with the event source
// (web, tcp, cron, ...). Call once per envelope processed.
func (m *Metrics) RecordEvent(ctx context.Context, source string, durationMs int64) {
	attrs := metric.WithAttributes(attribute.String(AttrEventsSource, source))
	m.EventsReceived.Add(ctx, 1, attrs)
	m.EventsResponseTimeMs.Record(ctx, durationMs, attrs)
}

// RecordOp increments the op.count counter and records the op.duration
// histogram, both tagged with the operation name.
func (m *Metrics) RecordOp(ctx context.Context, opName string, durationMs int64) {
	attrs := metric.WithAttributes(attribute.String(AttrOpName, opName))
	m.OpTotal.Add(ctx, 1, attrs)
	m.OpDurationMs.Record(ctx, durationMs, attrs)
}

// RecordSecretMaterialize increments the secret.materialize counter
// tagged with tenant slug + secret name. Nil-safe: if m or
// m.SecretMaterialized is nil (the chassis was built without
// metrics, e.g. some unit tests), this is a no-op. NEVER pass the
// cleartext or any portion of it — only the NAME.
func (m *Metrics) RecordSecretMaterialize(ctx context.Context, tenantSlug, secretName string) {
	if m == nil || m.SecretMaterialized == nil {
		return
	}
	m.SecretMaterialized.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrTenantSlug, tenantSlug),
		attribute.String(AttrSecretName, secretName),
	))
}

func buildResource(conf config.Config) *resource.Resource {
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "txco-chassis"
	}

	r, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName),
		semconv.DeploymentEnvironmentName(conf.Environment),
		semconv.HostName(conf.Fqdn),
	))
	if err != nil {
		// resource.Merge only fails on schema URL mismatch; safe fallback.
		return resource.Default()
	}
	return r
}

func otlpEnabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" ||
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}
