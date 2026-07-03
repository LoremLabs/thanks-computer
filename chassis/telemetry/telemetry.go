package telemetry

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// dropUnpinned is the reason for intents on a request whose tenant was
// never pinned (declared here rather than schema.go: it's a dispatch
// decision, not a validation one).
const dropUnpinned = "unpinned"

// warnInterval rate-limits per-(tenant,reason) zap warns so a hot
// misconfigured stack can't flood the log.
const warnInterval = time.Minute

// Processor is the request-end dispatcher: it reads metric intents off
// the final envelope, validates them, and hands survivors to the
// exporter. Construct once at server start; call Process from the
// request-completion path (after the response is written) and Close on
// shutdown. All methods are nil-receiver-safe so callers can hold a
// nil *Processor when the feature is off.
type Processor struct {
	exp     Exporter
	logger  *zap.Logger
	dropped DropFunc
	now     func() time.Time

	mu       sync.Mutex
	lastWarn map[string]time.Time
}

// NewProcessor builds a Processor around an opened Exporter. logger
// and dropped may be nil.
func NewProcessor(exp Exporter, logger *zap.Logger, dropped DropFunc) *Processor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Processor{
		exp:      exp,
		logger:   logger,
		dropped:  dropped,
		now:      time.Now,
		lastWarn: map[string]time.Time{},
	}
}

// Process handles one completed request. finalPayload is the final
// merged envelope; tenant must be the TRUSTED pinned slug ("" when the
// pipeline never pinned one — never the author-rewritable envelope
// field). Best-effort: drops are counted, nothing propagates back.
func (p *Processor) Process(ctx context.Context, finalPayload []byte, tenant, stack, src string) {
	if p == nil || p.exp == nil || len(finalPayload) == 0 {
		return
	}
	if !HasMetrics(finalPayload) {
		return // the common case: one gjson probe, nothing else
	}
	if tenant == "" {
		// Metric export runs with the tenant's credentials; without a
		// pinned tenant there is no trustworthy identity to attribute
		// the egress to, so the intents are dropped, visibly.
		n := rawMetricCount(finalPayload)
		p.dropped.Drop("", dropUnpinned, n)
		p.warn("", dropUnpinned, "telemetry intents on a request with no pinned tenant")
		return
	}

	events := ParseAndValidate(finalPayload, tenant, stack, src, p.now(), p.dropped)
	if len(events) == 0 {
		return
	}
	p.exp.Record(ctx, tenant, events)
}

// Close flushes the exporter, bounded by ctx.
func (p *Processor) Close(ctx context.Context) error {
	if p == nil || p.exp == nil {
		return nil
	}
	return p.exp.Close(ctx)
}

// warn emits at most one log line per (tenant, key) per warnInterval.
func (p *Processor) warn(tenant, key, msg string) {
	p.mu.Lock()
	k := tenant + "\x00" + key
	last, ok := p.lastWarn[k]
	nw := p.now()
	if ok && nw.Sub(last) < warnInterval {
		p.mu.Unlock()
		return
	}
	p.lastWarn[k] = nw
	p.mu.Unlock()
	p.logger.Warn(msg, zap.String("tenant", tenant), zap.String("reason", key))
}
