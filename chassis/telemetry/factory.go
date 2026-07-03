// Package telemetry processes tenant-owned business metrics.
//
// Application stacks EMIT metric intents into the request envelope at
// `_txc.telemetry.metrics` (an array of {name, kind, value, unit, attrs}
// objects; MergeJSON's array-append semantics accumulate contributions
// from multiple rules). After the request completes — off the response
// path — the chassis validates, redacts, and enriches the intents, then
// hands them to the configured Exporter. Export is best-effort: a
// failure can drop metrics (counted via DropFunc) but never affects the
// request.
//
// Enablement is per tenant, by convention: a tenant that sets the
// SecretEndpointName secret (`txco secrets set TELEMETRY_ENDPOINT`) has
// telemetry on; without it, intents are dropped. There is no config
// table — the endpoint and its auth headers ARE the configuration, and
// they are credentials, so the tenant secret store is where they live.
//
// Known v1 gaps, accepted deliberately: requests completed on the
// continuation/deferred paths bypass the request-end seam; intents are
// visible in trace artifacts like any other envelope field; only the
// tenant-wide secret scope is consulted (no per-stack override).
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"go.uber.org/zap"
)

// Conventional tenant secret names — the tenant-facing enablement
// contract. SecretEndpointName holds the destination URL (https
// required, plain http allowed for loopback); its presence turns the
// feature on for the tenant. SecretHeadersName is optional and holds
// request headers in the OTel env-var format: "k1=v1,k2=v2".
const (
	SecretEndpointName = "TELEMETRY_ENDPOINT"
	SecretHeadersName  = "TELEMETRY_HEADERS"
)

// ErrSecretNotFound is the seam's own not-found sentinel so backends
// never import the secret store; the server-side SecretSource adapter
// maps the store's not-found error to this one.
var ErrSecretNotFound = errors.New("telemetry: secret not found")

// SecretSource resolves one named tenant secret to its cleartext.
// Returns ErrSecretNotFound when the tenant hasn't set it. The returned
// slice is caller-owned; copy what you need and zero it.
type SecretSource func(ctx context.Context, tenantSlug, name string) ([]byte, error)

// DropFunc counts metric intents that were dropped rather than
// exported, tagged with a small fixed reason vocabulary. May be nil —
// invoke it through Drop.
type DropFunc func(tenantSlug, reason string, n int64)

// Drop is the nil-safe way to invoke a DropFunc.
func (d DropFunc) Drop(tenantSlug, reason string, n int64) {
	if d != nil && n > 0 {
		d(tenantSlug, reason, n)
	}
}

// MetricEvent is one validated, enriched metric ready for export.
type MetricEvent struct {
	Tenant string
	Stack  string
	Src    string

	Name  string
	Kind  string // "counter" | "histogram"
	Value float64
	Unit  string

	// Attrs values are string, float64, or bool only (schema.go
	// enforces this before an event is built).
	Attrs map[string]any
	Time  time.Time
}

// Exporter delivers validated metric events for one tenant at a time.
// Record must be best-effort and quick: buffer/aggregate in memory and
// ship in the background; never block on the network. Close flushes
// whatever is pending, bounded by the caller's context.
type Exporter interface {
	Name() string
	Record(ctx context.Context, tenant string, events []MetricEvent)
	Close(ctx context.Context) error
}

// ExporterConfig carries the node/runtime context a backend may need,
// resolved from chassis config. It is deliberately generic — the same
// posture as usage.SinkConfig — so the seam stays unopinionated; a
// backend reads any backend-specific settings in its constructor.
type ExporterConfig struct {
	// NodeID is a stable identity for this chassis (FQDN-ish), for
	// attribution when many nodes export the same tenant's metrics.
	NodeID string
	// Environment is the chassis environment (dev/stage/prod).
	Environment string
	// Logger is the chassis logger; a backend may emit observability
	// lines but must never log secret values.
	Logger *zap.Logger
	// HTTPClient is the egress-guarded outbound client a network
	// backend must use for tenant-supplied destinations.
	HTTPClient *http.Client
	// Secrets resolves per-tenant configuration secrets.
	Secrets SecretSource
	// Dropped counts metric intents dropped instead of exported.
	Dropped DropFunc
}

// Constructor builds an Exporter from resolved config. Called by Open.
type Constructor func(ExporterConfig) (Exporter, error)

// backends maps exporter name → constructor. The bundled backends
// ("otlp", "log") self-register from their packages' init(); the
// chassis activates one with a blank import + config selection.
var backends = map[string]Constructor{}

// Register adds an exporter constructor. Called from a backend
// package's init().
func Register(name string, c Constructor) {
	backends[name] = c
}

// Open constructs the named exporter. Unknown name is a startup error
// listing what is available (sorted for a stable message).
func Open(name string, cfg ExporterConfig) (Exporter, error) {
	c, ok := backends[name]
	if !ok {
		avail := make([]string, 0, len(backends))
		for k := range backends {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("telemetry: unknown exporter %q (available: %v)", name, avail)
	}
	return c(cfg)
}
