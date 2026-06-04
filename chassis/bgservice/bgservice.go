// Package bgservice is the chassis seam for long-running background services
// that an overlay registers and the chassis owns — started with the other
// controllers and drained on shutdown. It mirrors the usage.Sink registry seam:
// a backend self-registers from its init() and the chassis activates it with a
// blank import plus a name in the BackgroundServices config. The seam is
// deliberately generic — no billing/quota/transport vocabulary — so it stays
// unopinionated; a service reads any backend-specific settings (DSNs, periods,
// thresholds) from its own env in its constructor.
package bgservice

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// Service is a long-running background loop the chassis owns. Start launches it
// (non-blocking); Stop drains it on shutdown. The shape matches the chassis
// controller contract so a Service plugs into the existing start/stop loops.
type Service interface {
	Start()
	Stop()
}

// Gate is the seam a background service uses to drive per-tenant admission
// state without touching DB internals. It engages or releases the programmatic
// gate (the `suspended` column) for a tenant identified by slug, with a
// caller-supplied deny_reason/deny_status, routing through the chassis'
// full-row read-modify-write + fleet-emit + reload path. Billing-neutral: the
// specific meaning rides in deny_reason (e.g. "credit_exhausted").
type Gate interface {
	SetGate(ctx context.Context, slug string, suspended bool, denyStatus int, denyReason string) error
}

// Config is the generic context a service receives at construction.
type Config struct {
	// Logger is the chassis logger.
	Logger *zap.Logger
	// Gate drives per-tenant admission state (see Gate).
	Gate Gate
	// NodeID is a stable identity for this chassis (FQDN-ish), for attribution.
	NodeID string
}

// Constructor builds a Service from resolved config. Called by Open.
type Constructor func(Config) (Service, error)

// registry maps service name → constructor. A backend self-registers from its
// own init(); the chassis activates it with a blank import.
var registry = map[string]Constructor{}

// Register adds a service constructor. Called from a backend package's init().
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the services named in a comma-separated list (whitespace and
// empty/"nop" entries ignored). An empty list returns no services — the bundled
// default, so single-node boots register nothing. An unknown name is a startup
// error listing what is available (sorted for a stable message).
func Open(names string, cfg Config) ([]Service, error) {
	var out []Service
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" || name == "nop" {
			continue
		}
		c, ok := registry[name]
		if !ok {
			avail := make([]string, 0, len(registry))
			for k := range registry {
				avail = append(avail, k)
			}
			sort.Strings(avail)
			return nil, fmt.Errorf("bgservice: unknown service %q (available: %v)", name, avail)
		}
		svc, err := c(cfg)
		if err != nil {
			return nil, fmt.Errorf("bgservice: open %q: %w", name, err)
		}
		if svc != nil {
			out = append(out, svc)
		}
	}
	return out, nil
}
