// Package log is the bundled "log" telemetry exporter: every validated
// metric event becomes one structured chassis log line. It deliberately
// ignores the tenant's TELEMETRY_* secrets — every tenant is "enabled" —
// so a developer can watch metrics flow on a dev chassis without
// configuring an endpoint. No network, no state.
package log

import (
	"context"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/telemetry"
)

func init() {
	telemetry.Register("log", func(cfg telemetry.ExporterConfig) (telemetry.Exporter, error) {
		logger := cfg.Logger
		if logger == nil {
			logger = zap.NewNop()
		}
		return &sink{logger: logger}, nil
	})
}

type sink struct{ logger *zap.Logger }

func (s *sink) Name() string { return "log" }

func (s *sink) Record(_ context.Context, tenant string, events []telemetry.MetricEvent) {
	for _, ev := range events {
		s.logger.Info("tenant metric",
			zap.String("tenant", tenant),
			zap.String("stack", ev.Stack),
			zap.String("src", ev.Src),
			zap.String("name", ev.Name),
			zap.String("kind", ev.Kind),
			zap.Float64("value", ev.Value),
			zap.String("unit", ev.Unit),
			zap.Any("attrs", ev.Attrs),
		)
	}
}

func (s *sink) Close(context.Context) error { return nil }
