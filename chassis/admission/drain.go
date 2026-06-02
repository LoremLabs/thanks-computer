package admission

import (
	"net/http"
	"sync/atomic"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// draining is the process-level drain flag. It is global by design:
// drain is a property of THIS node (a SIGUSR1 tells the process to bleed
// out of its load balancer), not of any tenant or request.
var draining atomic.Bool

// SetDraining turns the node's drain state on or off. Idempotent. Wired
// to SIGUSR1 (on) / SIGUSR2 (off) by the app signal loop.
func SetDraining(on bool) { draining.Store(on) }

// IsDraining reports whether the node is draining. Read by the health
// endpoint (returns 503 so the load balancer removes the node) and by the
// server bus loop (new requests get a 503 terminal response while
// in-flight requests are allowed to finish).
func IsDraining() bool { return draining.Load() }

// DrainResponse shapes resp into a 503 "draining" terminal response,
// reusing ShapeDeny's web shaping and adding Retry-After + Connection:
// close so clients and proxies don't pin a draining node.
func DrainResponse(resp string) string {
	out := ShapeDeny(resp, Decision{Status: http.StatusServiceUnavailable, Reason: "draining"}, "")
	if gjson.Get(out, "_txc.web.req").Exists() {
		out, _ = sjson.Set(out, "_txc.web.res.headers.retry-after.0", "0")
		out, _ = sjson.Set(out, "_txc.web.res.headers.connection.0", "close")
	}
	return out
}
