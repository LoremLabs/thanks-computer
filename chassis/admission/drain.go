package admission

import (
	"net/http"
	"sync/atomic"
)

// draining is the process-level drain flag. It is global by design: drain
// is a property of THIS node (a SIGUSR1 tells the process to bleed out of
// its load balancer), not of any tenant or request.
var draining atomic.Bool

// SetDraining turns the node's drain state on or off. Idempotent. Wired to
// SIGUSR1 (on) / SIGUSR2 (off) by the app signal loop.
func SetDraining(on bool) { draining.Store(on) }

// IsDraining reports whether the node is draining. Read by the health
// endpoint (returns 503 so the load balancer removes the node) and by the
// server bus loop (new requests get a 503 terminal response while
// in-flight requests are allowed to finish).
func IsDraining() bool { return draining.Load() }

// DrainResponse stamps the transport-neutral admission marker for a 503
// "draining" denial. Each outlet renders it in its own protocol (web →
// 503 + Retry-After; lmtp → 451 so mail retries; tcp → close). Used by the
// server bus loop while the node is draining.
func DrainResponse(resp string) string {
	return MarkDenied(resp, Decision{Status: http.StatusServiceUnavailable, Reason: "draining"}, "")
}
