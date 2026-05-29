package admin

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
)

// Defaults: at most 10 attempts per 60 seconds per IP on the
// unsigned credential-mint endpoints. Sized to block brute-force
// probing of 96-bit secrets while remaining well above any
// legitimate retry rate (the CLI doesn't loop on failures).
const (
	defaultThrottleRate   = 10
	defaultThrottleWindow = time.Minute
)

// newThrottleFromEnv builds the throttle the admin controller uses
// for unsigned endpoints. Reads three optional env knobs:
//
//	TXCO_THROTTLE_RATE      int     — attempts per window per IP
//	TXCO_THROTTLE_WINDOW    duration — Go duration string ("1m", "30s")
//	TXCO_THROTTLE_DISABLED  any non-empty value disables (rate=0)
//
// Misconfigured values fall back to defaults rather than failing
// startup; the chassis logs the effective values via Start so an
// operator can confirm what's running.
func newThrottleFromEnv() *throttle.Throttle {
	if os.Getenv("TXCO_THROTTLE_DISABLED") != "" {
		return throttle.New(0, 0)
	}
	rate := defaultThrottleRate
	if v := os.Getenv("TXCO_THROTTLE_RATE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			rate = n
		}
	}
	window := defaultThrottleWindow
	if v := os.Getenv("TXCO_THROTTLE_WINDOW"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			window = d
		}
	}
	return throttle.New(rate, window)
}

// throttleMiddleware wraps a handler with per-IP throttling. The
// counter sits between the auth middleware and the handler — but
// since the unsigned endpoints aren't behind auth, this is really
// the only gate before reaching the handler body. Wired in Start
// only for /auth/dev/enroll and /auth/invitations/consume.
//
// Counts every admitted request, regardless of outcome. A 401 (bad
// token) and a 200 (successful consume) both consume budget;
// otherwise an attacker could probe via timing alone. The 429 itself
// does NOT increment — we already counted when we admitted them.
func (c *Controller) throttleMiddleware(t *throttle.Throttle) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := clientIP(r)
			if ok, retryAfter := t.Allow(key); !ok {
				retrySec := int(retryAfter.Seconds())
				if retrySec < 1 {
					retrySec = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				writeJSONError(w, http.StatusTooManyRequests, "throttled",
					map[string]any{"retry_after_seconds": retrySec})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the throttle key for an incoming request: the
// peer's IP address with the port stripped. We deliberately do NOT
// honor X-Forwarded-For — operators behind a TLS-terminating proxy
// will see every request coming from the proxy IP, which means the
// throttle bites ALL their callers (wrong, but conservative). When
// someone hits that and complains, we add a trusted-proxy flag; the
// default stays "trust the kernel."
//
// Falls back to the raw RemoteAddr when SplitHostPort fails (e.g.
// a unix socket peer) so the throttle still has a deterministic key.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
