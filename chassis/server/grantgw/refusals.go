package grantgw

import (
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

// refusals remembers a refusal for a short window, so a program that asks
// again and again for something it was refused cannot run the tenant's
// `_grant` stack without end. Only refusals are remembered: every release
// is decided afresh.
//
// The cost is that a change which would now allow the request — a new pull
// policy, a new rule — takes up to the window to be seen by a run that was
// just refused.
type refusals struct {
	window time.Duration
	max    int

	mu   sync.Mutex
	seen map[string]refusal
}

type refusal struct {
	reason string
	until  time.Time
}

const maxRefusals = 4096

func newRefusals(window time.Duration) *refusals {
	return &refusals{window: window, max: maxRefusals, seen: map[string]refusal{}}
}

func refusalKey(grantID string, res authn.Resource) string {
	return grantID + "\x00" + string(res.Kind) + "\x00" + res.Name
}

// get returns the remembered reason a request was refused, if it still holds.
func (r *refusals) get(key string, now time.Time) (string, bool) {
	if r == nil || r.window <= 0 {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.seen[key]
	if !ok {
		return "", false
	}
	if !now.Before(e.until) {
		delete(r.seen, key)
		return "", false
	}
	return e.reason, true
}

// put remembers a refusal. When full it drops what has lapsed, and if that
// frees nothing, everything: forgetting a refusal costs one more run of the
// stack, never a release.
func (r *refusals) put(key, reason string, now time.Time) {
	if r == nil || r.window <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.seen) >= r.max {
		for k, e := range r.seen {
			if !now.Before(e.until) {
				delete(r.seen, k)
			}
		}
		if len(r.seen) >= r.max {
			r.seen = map[string]refusal{}
		}
	}
	r.seen[key] = refusal{reason: reason, until: now.Add(r.window)}
}
