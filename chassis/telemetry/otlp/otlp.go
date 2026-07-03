// Package otlp is the bundled "otlp" telemetry exporter: each tenant's
// validated metric events feed a tenant-private OTel MeterProvider
// whose periodic reader ships OTLP/HTTP batches to the endpoint named
// by the tenant's TELEMETRY_ENDPOINT secret (auth headers from the
// optional TELEMETRY_HEADERS secret, OTel "k1=v1,k2=v2" format).
//
// Per-tenant providers are cached and re-validated on a TTL: rotating
// the secrets swaps in a fresh provider (the old one flushes to the old
// destination on its way out), deleting them turns the tenant off, and
// a tenant without secrets costs one negative-cache lookup per TTL.
// All delivery is best-effort — export failures count toward the
// chassis telemetry.dropped diagnostic and never surface to requests.
//
// Every option that otlpmetrichttp can read from OTEL_EXPORTER_OTLP_*
// env vars is passed EXPLICITLY here (endpoint, headers), because those
// env vars configure the chassis's own telemetry on a production node —
// inheriting them would aim chassis credentials at tenant-controlled
// destinations.
package otlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/telemetry"
)

const (
	// resolveTTL bounds how stale a tenant's cached exporter config can
	// get: secret rotation/deletion takes effect within this window.
	resolveTTL = 5 * time.Minute
	// errorRetryTTL is the short recheck window after a transient
	// secret-store error, so a hiccup neither hammers the store per
	// request nor sticks for a full TTL.
	errorRetryTTL = 30 * time.Second
	// exportInterval is the periodic reader's flush cadence.
	exportInterval = 30 * time.Second
	// exportTimeout bounds one OTLP request.
	exportTimeout = 10 * time.Second
	// swapShutdownTimeout bounds the final flush of a replaced provider.
	swapShutdownTimeout = 10 * time.Second

	scopeName = "github.com/loremlabs/thanks-computer/chassis/telemetry/otlp"
)

// Drop reasons produced by this backend.
const (
	dropDisabled        = "disabled"
	dropBadEndpoint     = "bad_endpoint"
	dropSecretError     = "secret_error"
	dropExportError     = "export_error"
	dropInstrumentError = "instrument_error"
)

func init() {
	telemetry.Register("otlp", New)
}

// New builds the otlp exporter. Exported for tests; production code
// goes through telemetry.Open("otlp", cfg).
func New(cfg telemetry.ExporterConfig) (telemetry.Exporter, error) {
	if cfg.HTTPClient == nil {
		return nil, errors.New("telemetry/otlp: ExporterConfig.HTTPClient is required (must be the egress-guarded client)")
	}
	if cfg.Secrets == nil {
		return nil, errors.New("telemetry/otlp: ExporterConfig.Secrets is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	return &exporter{
		cfg:      cfg,
		tenants:  map[string]*tenantEntry{},
		ttl:      resolveTTL,
		errTTL:   errorRetryTTL,
		interval: exportInterval,
		now:      time.Now,
	}, nil
}

// tenantEntry is one tenant's cached exporter state. provider == nil is
// the negative cache: the tenant is off (no secret / bad endpoint /
// resolve error), with dropReason saying why.
type tenantEntry struct {
	provider   *sdkmetric.MeterProvider
	meter      metric.Meter
	cfgHash    string
	dropReason string
	recheckAt  time.Time
}

type exporter struct {
	cfg telemetry.ExporterConfig

	mu      sync.Mutex
	tenants map[string]*tenantEntry
	closed  bool

	// swapWg tracks background shutdowns of replaced providers so Close
	// can drain their final flushes too.
	swapWg sync.WaitGroup

	// Test seams.
	ttl      time.Duration
	errTTL   time.Duration
	interval time.Duration
	now      func() time.Time
}

func (e *exporter) Name() string { return "otlp" }

// Record hands one request's validated events to the tenant's meter.
// All work is in-memory (the SDK aggregates; the periodic reader ships
// batches in the background) except a possible secret resolve when the
// tenant's cache entry is missing or past its TTL.
func (e *exporter) Record(ctx context.Context, tenant string, events []telemetry.MetricEvent) {
	ent := e.entryFor(ctx, tenant)
	if ent == nil {
		e.cfg.Dropped.Drop(tenant, dropDisabled, int64(len(events)))
		return
	}
	if ent.provider == nil {
		e.cfg.Dropped.Drop(tenant, ent.dropReason, int64(len(events)))
		return
	}
	for _, ev := range events {
		attrs := metric.WithAttributes(otelAttrs(ev)...)
		switch ev.Kind {
		case "counter":
			c, err := ent.meter.Float64Counter(ev.Name, metric.WithUnit(ev.Unit))
			if err != nil {
				// e.g. the same name already registered as a histogram.
				e.cfg.Dropped.Drop(tenant, dropInstrumentError, 1)
				continue
			}
			c.Add(ctx, ev.Value, attrs)
		case "histogram":
			h, err := ent.meter.Float64Histogram(ev.Name, metric.WithUnit(ev.Unit))
			if err != nil {
				e.cfg.Dropped.Drop(tenant, dropInstrumentError, 1)
				continue
			}
			h.Record(ctx, ev.Value, attrs)
		}
	}
}

// Close flushes and shuts down every live provider (plus any in-flight
// replaced-provider flushes), bounded by ctx.
func (e *exporter) Close(ctx context.Context) error {
	e.mu.Lock()
	e.closed = true
	providers := make([]*sdkmetric.MeterProvider, 0, len(e.tenants))
	for _, ent := range e.tenants {
		if ent.provider != nil {
			providers = append(providers, ent.provider)
		}
	}
	e.tenants = map[string]*tenantEntry{}
	e.mu.Unlock()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	for _, p := range providers {
		wg.Add(1)
		go func(p *sdkmetric.MeterProvider) {
			defer wg.Done()
			if err := p.Shutdown(ctx); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(p)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		e.swapWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return firstErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// entryFor returns the tenant's cache entry, (re-)resolving its secrets
// when missing or past recheckAt. The resolve happens under mu — it is
// a covered SQLite point read, so briefly serializing other tenants'
// Records is cheaper than per-tenant lock plumbing.
func (e *exporter) entryFor(ctx context.Context, tenant string) *tenantEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	nw := e.now()
	ent, ok := e.tenants[tenant]
	if ok && nw.Before(ent.recheckAt) {
		return ent
	}

	endpoint, headers, err := e.resolve(ctx, tenant)
	switch {
	case errors.Is(err, telemetry.ErrSecretNotFound):
		// Cleanly off. If it was on, the secret was deleted — retire the
		// provider with a final flush to the old destination.
		if ok && ent.provider != nil {
			e.shutdownReplaced(ent.provider)
		}
		ent = &tenantEntry{dropReason: dropDisabled, recheckAt: nw.Add(e.ttl)}
		e.tenants[tenant] = ent
		return ent
	case err != nil:
		if ok {
			// Stale-while-error: keep exporting with the config we have;
			// just retry the resolve sooner.
			ent.recheckAt = nw.Add(e.errTTL)
			return ent
		}
		e.warnTenant(tenant, "telemetry secret resolve failed", err)
		ent = &tenantEntry{dropReason: dropSecretError, recheckAt: nw.Add(e.errTTL)}
		e.tenants[tenant] = ent
		return ent
	}

	hash := cfgHash(endpoint, headers)
	if ok && ent.provider != nil && ent.cfgHash == hash {
		ent.recheckAt = nw.Add(e.ttl) // unchanged — keep the provider
		return ent
	}

	provider, meter, err := e.build(tenant, endpoint, headers)
	if err != nil {
		if ok && ent.provider != nil {
			e.shutdownReplaced(ent.provider)
		}
		// Never log the endpoint or error verbatim — a malformed URL may
		// embed credentials. The reason string is enough to debug with.
		e.cfg.Logger.Warn("telemetry endpoint rejected",
			zap.String("tenant", tenant), zap.String("reason", err.Error()))
		ent = &tenantEntry{dropReason: dropBadEndpoint, recheckAt: nw.Add(e.ttl)}
		e.tenants[tenant] = ent
		return ent
	}
	if ok && ent.provider != nil {
		e.shutdownReplaced(ent.provider) // rotation: flush to the old destination
	}
	ent = &tenantEntry{provider: provider, meter: meter, cfgHash: hash, recheckAt: nw.Add(e.ttl)}
	e.tenants[tenant] = ent
	return ent
}

// resolve materializes the tenant's telemetry secrets. The endpoint is
// required (its absence disables the tenant); headers are optional and
// default to an explicit empty map — never nil, so the exporter always
// overrides any ambient OTEL_EXPORTER_OTLP_HEADERS.
func (e *exporter) resolve(ctx context.Context, tenant string) (string, map[string]string, error) {
	epRaw, err := e.cfg.Secrets(ctx, tenant, telemetry.SecretEndpointName)
	if err != nil {
		return "", nil, err
	}
	endpoint := strings.TrimSpace(string(epRaw))
	zero(epRaw)

	headers := map[string]string{}
	hRaw, err := e.cfg.Secrets(ctx, tenant, telemetry.SecretHeadersName)
	switch {
	case err == nil:
		headers = parseHeaders(string(hRaw))
		zero(hRaw)
	case errors.Is(err, telemetry.ErrSecretNotFound):
		// optional — fine
	default:
		return "", nil, err
	}
	return endpoint, headers, nil
}

// build validates the endpoint and constructs the tenant's provider.
// No network happens here; the first export is the periodic reader's.
func (e *exporter) build(tenant, endpoint string, headers map[string]string) (*sdkmetric.MeterProvider, metric.Meter, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, nil, err
	}

	exp, err := otlpmetrichttp.New(context.Background(),
		otlpmetrichttp.WithEndpointURL(endpoint),
		otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithHTTPClient(e.cfg.HTTPClient),
		otlpmetrichttp.WithTimeout(exportTimeout),
	)
	if err != nil {
		return nil, nil, err
	}

	// resource.NewWithAttributes (not resource.Default) so nothing from
	// the node's OTEL_* env — chassis identity — rides tenant streams.
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(tenant),
		attribute.String("txco.tenant", tenant),
		attribute.String("txco.node", e.cfg.NodeID),
		attribute.String("txco.environment", e.cfg.Environment),
	)

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(
			&accountingExporter{Exporter: exp, tenant: tenant, dropped: e.cfg.Dropped, logger: e.cfg.Logger, now: e.now},
			sdkmetric.WithInterval(e.interval),
		)),
		sdkmetric.WithResource(res),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
	)
	return provider, provider.Meter(scopeName), nil
}

// shutdownReplaced retires a provider in the background: its Shutdown
// force-flushes pending aggregations to the destination it was built
// for, so rotation loses nothing. Tracked so Close can drain stragglers.
func (e *exporter) shutdownReplaced(p *sdkmetric.MeterProvider) {
	e.swapWg.Add(1)
	go func() {
		defer e.swapWg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), swapShutdownTimeout)
		defer cancel()
		_ = p.Shutdown(ctx)
	}()
}

func (e *exporter) warnTenant(tenant, msg string, err error) {
	e.cfg.Logger.Warn(msg, zap.String("tenant", tenant), zap.Error(err))
}

// accountingExporter decorates the OTLP exporter so per-tenant export
// failures land in the chassis telemetry.dropped diagnostic instead of
// vanishing into the SDK's global error handler.
type accountingExporter struct {
	sdkmetric.Exporter
	tenant  string
	dropped telemetry.DropFunc
	logger  *zap.Logger

	mu       sync.Mutex
	lastWarn time.Time
	now      func() time.Time
}

func (a *accountingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	err := a.Exporter.Export(ctx, rm)
	if err != nil {
		var n int64
		for _, sm := range rm.ScopeMetrics {
			n += int64(len(sm.Metrics))
		}
		if n == 0 {
			n = 1
		}
		a.dropped.Drop(a.tenant, dropExportError, n)

		a.mu.Lock()
		nw := a.now()
		warn := nw.Sub(a.lastWarn) >= time.Minute
		if warn {
			a.lastWarn = nw
		}
		a.mu.Unlock()
		if warn && a.logger != nil {
			a.logger.Warn("telemetry export failed",
				zap.String("tenant", a.tenant), zap.Error(err))
		}
	}
	return err
}

// validateEndpoint enforces the destination policy: a well-formed
// https URL (plain http only toward loopback, for local collectors),
// no userinfo. The egress guard separately vetoes forbidden networks
// at dial time.
func validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("empty endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("unparseable endpoint URL")
	}
	if u.User != nil {
		return errors.New("endpoint must not carry userinfo")
	}
	if u.Host == "" {
		return errors.New("endpoint missing host")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
		return errors.New("http endpoint allowed only for loopback")
	default:
		return errors.New("endpoint scheme must be https")
	}
}

// parseHeaders reads the OTel env-var header format: comma-separated
// key=value pairs ("k1=v1,k2=v2"). Malformed pairs are skipped rather
// than failing the tenant's whole config.
func parseHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// cfgHash fingerprints (endpoint, headers) so a TTL recheck can tell
// "unchanged" from "rotated" without keeping cleartext around.
func cfgHash(endpoint string, headers map[string]string) string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(endpoint))
	for _, k := range keys {
		h.Write([]byte{0})
		h.Write([]byte(k))
		h.Write([]byte{1})
		h.Write([]byte(headers[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func otelAttrs(ev telemetry.MetricEvent) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(ev.Attrs)+2)
	attrs = append(attrs,
		attribute.String("txco.stack", ev.Stack),
		attribute.String("txco.src", ev.Src),
	)
	for k, v := range ev.Attrs {
		switch t := v.(type) {
		case string:
			attrs = append(attrs, attribute.String(k, t))
		case float64:
			attrs = append(attrs, attribute.Float64(k, t))
		case bool:
			attrs = append(attrs, attribute.Bool(k, t))
		}
	}
	return attrs
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
