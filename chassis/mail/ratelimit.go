package mail

import (
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateRule is one "max sends per window" cap.
type rateRule struct {
	max    int
	window time.Duration
}

// rateLimiter is a per-tenant sliding-window-log limiter enforcing a set of
// rules: a send is allowed only if EVERY rule is under its cap. It is in-memory
// and therefore PER NODE — caps reset on restart and the fleet total is roughly
// (cap × node count). It's a runaway-loop / abuse safety valve, not fleet-wide
// accounting (that's a future shared-Postgres phase).
type rateLimiter struct {
	rules   []rateRule
	longest time.Duration
	mu      sync.Mutex
	seen    map[string][]time.Time // tenant → ascending send timestamps within `longest`
}

// parseRateRules parses a spec like "100/2m,200/4h" into rules. Each entry is
// <count>/<go-duration>. Malformed or non-positive entries are skipped; an
// empty/all-bad spec yields nil (limiter disabled).
func parseRateRules(spec string) []rateRule {
	var rules []rateRule
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		slash := strings.IndexByte(part, '/')
		if slash <= 0 {
			continue
		}
		max, err := strconv.Atoi(strings.TrimSpace(part[:slash]))
		if err != nil || max <= 0 {
			continue
		}
		win, err := time.ParseDuration(strings.TrimSpace(part[slash+1:]))
		if err != nil || win <= 0 {
			continue
		}
		rules = append(rules, rateRule{max: max, window: win})
	}
	return rules
}

// newRateLimiter returns a limiter for the rules, or nil if there are none
// (so callers can cheaply gate on `rl != nil`).
func newRateLimiter(rules []rateRule) *rateLimiter {
	if len(rules) == 0 {
		return nil
	}
	var longest time.Duration
	for _, r := range rules {
		if r.window > longest {
			longest = r.window
		}
	}
	return &rateLimiter{rules: rules, longest: longest, seen: map[string][]time.Time{}}
}

// allow records one send for tenant at `now` and returns true iff every rule
// permits it. If any rule is at its cap it records nothing and returns false,
// so a throttled attempt does not itself consume budget. Assumes `now` is
// non-decreasing across calls (wall clock), which keeps the per-tenant log
// sorted ascending.
func (rl *rateLimiter) allow(tenant string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	ts := rl.seen[tenant]
	// Drop timestamps older than the longest window (bounds the slice to ~the
	// largest cap and keeps the per-rule counts cheap).
	cutoff := now.Add(-rl.longest)
	drop := 0
	for drop < len(ts) && ts[drop].Before(cutoff) {
		drop++
	}
	ts = ts[drop:]

	for _, r := range rl.rules {
		start := now.Add(-r.window)
		count := 0
		for i := len(ts) - 1; i >= 0 && !ts[i].Before(start); i-- {
			count++
		}
		if count >= r.max {
			if len(ts) == 0 {
				delete(rl.seen, tenant)
			} else {
				rl.seen[tenant] = ts
			}
			return false
		}
	}

	rl.seen[tenant] = append(ts, now)
	return true
}
