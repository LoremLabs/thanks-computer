package admission

import (
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

const defaultDenyStatus = 403

// Provider answers the per-tenant admission questions on the request hot
// path. Implementations must be lock-light and non-blocking — no DB or
// network I/O per request.
type Provider interface {
	// Decide returns the suspend/enable decision for a tenant slug. An
	// unknown tenant (no row) and the system tenant are always admitted.
	Decide(tenant string) Decision
	// AllowRate reports whether the tenant is under its node-local rate
	// limit; retry is a suggested Retry-After when denied. No limit
	// configured (rps==0) / unknown tenant => allow.
	AllowRate(tenant string) (allow bool, retry time.Duration)
	// AcquireConcurrency takes a node-local concurrency slot for the
	// tenant, registering its release on lease. No limit (cap==0) / unknown
	// tenant => admit without taking a slot. Returns false when at capacity.
	AcquireConcurrency(tenant string, lease *Lease) bool
}

// NopProvider admits everyone. The nil/test/disabled default so call sites
// can stay unconditional.
type NopProvider struct{}

func (NopProvider) Decide(string) Decision                 { return Decision{Admit: true} }
func (NopProvider) AllowRate(string) (bool, time.Duration) { return true, 0 }
func (NopProvider) AcquireConcurrency(string, *Lease) bool { return true }

// concState is one tenant's live concurrency counter. All fields are
// atomics: acquire/release are lock-free; the provider mutex is held only
// to look the *concState up in the map (create/evict), never during the
// CAS loop. cap==0 means unlimited.
type concState struct {
	inFlight atomic.Int64
	cap      atomic.Int64
}

// sqliteProvider serves admission decisions from an in-memory snapshot of
// tenant_runtime_state. Decide reads an immutable atomic snapshot (lock-
// free). Rate limiters + concurrency counters are MUTABLE per-tenant state
// kept in mutex-guarded maps, updated in place on reload (so live in-flight
// counts survive a config-apply). Entries exist ONLY for configured tenants
// (rps>0 / cap>0), so the maps are bounded by configured tenants — an
// unknown/attacker hostname never allocates an entry.
type sqliteProvider struct {
	snap   atomic.Pointer[map[string]TenantRuntimeState] // key = tenant slug
	logger *zap.Logger

	mu sync.Mutex               // guards the rl/cc MAPS (lookup/create/evict)
	rl map[string]*rate.Limiter // tenants with rate_limit_rps > 0
	cc map[string]*concState    // tenants with concurrency_limit > 0
}

// NewSQLiteProvider returns a provider seeded with empty state, so reads
// before the first Rebuild admit everyone.
func NewSQLiteProvider(logger *zap.Logger) *sqliteProvider {
	p := &sqliteProvider{
		logger: logger,
		rl:     map[string]*rate.Limiter{},
		cc:     map[string]*concState{},
	}
	empty := map[string]TenantRuntimeState{}
	p.snap.Store(&empty)
	return p
}

// Decide implements Provider. The system tenant and unknown tenants are
// admitted; a present row admits unless disabled/suspended, in which case
// the row's deny_status (default 403) and reason are returned.
func (p *sqliteProvider) Decide(tenant string) Decision {
	if tenant == "" || tenant == tenants.SystemTenantSlug {
		return Decision{Admit: true}
	}
	m := p.snap.Load()
	if m == nil {
		return Decision{Admit: true}
	}
	st, ok := (*m)[tenant]
	if !ok || st.Admitted() {
		return Decision{Admit: true}
	}
	status := st.DenyStatus
	if status == 0 {
		status = defaultDenyStatus
	}
	return Decision{Admit: false, Status: status, Reason: st.DenyReason}
}

// AllowRate implements Provider via a reservation-first check: Reserve()
// mutates the bucket, so call it once (never Allow()+Reserve()). delay==0
// means the token was taken (allow); otherwise cancel the reservation to
// return the token and deny with delay as the Retry-After.
func (p *sqliteProvider) AllowRate(tenant string) (bool, time.Duration) {
	if tenant == "" || tenant == tenants.SystemTenantSlug {
		return true, 0
	}
	p.mu.Lock()
	lim := p.rl[tenant]
	p.mu.Unlock()
	if lim == nil {
		return true, 0 // no rate limit configured
	}
	r := lim.Reserve()
	if !r.OK() {
		return false, time.Second // unsatisfiable (burst < 1) — shouldn't happen
	}
	d := r.Delay()
	if d == 0 {
		return true, 0 // token taken
	}
	r.Cancel() // give the token back; we don't wait
	return false, d
}

// AcquireConcurrency implements Provider with a lock-free CAS loop on the
// tenant's in-flight counter. The mutex is held only to fetch the
// *concState; the counter math runs unlocked. On success the slot's
// decrement is registered on lease (released by the bus-loop defer).
func (p *sqliteProvider) AcquireConcurrency(tenant string, lease *Lease) bool {
	if tenant == "" || tenant == tenants.SystemTenantSlug {
		return true
	}
	p.mu.Lock()
	cs := p.cc[tenant]
	p.mu.Unlock()
	if cs == nil {
		return true // no concurrency limit configured
	}
	limit := cs.cap.Load()
	if limit <= 0 {
		return true // unlimited => admit, register nothing
	}
	for {
		cur := cs.inFlight.Load()
		if cur >= limit {
			return false // at capacity
		}
		if cs.inFlight.CompareAndSwap(cur, cur+1) {
			lease.onRelease(func() { cs.inFlight.Add(-1) })
			return true
		}
	}
}

// Rebuild replaces the Decide snapshot AND updates the rate/concurrency maps
// from the given handle. On ANY error (including the table being absent
// before 0014) the previous state is kept untouched — admission never goes
// dark, and live in-flight counters are never lost.
func (p *sqliteProvider) Rebuild(db *sql.DB) error {
	if p == nil || db == nil {
		return nil
	}
	rows, err := db.Query(`
        SELECT t.slug, s.enabled, s.suspended, s.deny_status, s.deny_reason,
               s.rate_limit_rps, s.rate_burst, s.concurrency_limit
        FROM tenant_runtime_state s
        JOIN tenants t ON t.tenant_id = s.tenant_id
        WHERE t.revoked_at IS NULL`)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("admission provider rebuild: query failed", zap.Error(err))
		}
		return err
	}
	defer rows.Close()

	out := map[string]TenantRuntimeState{}
	for rows.Next() {
		var (
			slug       string
			enabled    int
			suspended  int
			denyStatus int
			denyReason string
			rps        float64
			burst      int
			conc       int
		)
		if err := rows.Scan(&slug, &enabled, &suspended, &denyStatus, &denyReason, &rps, &burst, &conc); err != nil {
			if p.logger != nil {
				p.logger.Warn("admission provider rebuild: scan", zap.Error(err))
			}
			return err
		}
		out[slug] = TenantRuntimeState{
			Enabled:          enabled != 0,
			Suspended:        suspended != 0,
			DenyStatus:       denyStatus,
			DenyReason:       denyReason,
			RateLimitRPS:     rps,
			RateBurst:        burst,
			ConcurrencyLimit: conc,
		}
	}
	if err := rows.Err(); err != nil {
		if p.logger != nil {
			p.logger.Warn("admission provider rebuild: rows", zap.Error(err))
		}
		return err
	}
	p.snap.Store(&out)
	p.syncLimiters(out)
	if p.logger != nil {
		p.logger.Debug("admission provider rebuilt", zap.Int("tenants", len(out)))
	}
	return nil
}

// syncLimiters updates the mutable rate/concurrency maps IN PLACE from the
// new snapshot — never rebuilt-and-swapped, or live inFlight counters would
// be lost. Token state and in-flight counts are preserved; only the rate /
// burst / cap parameters change. Entries for tenants no longer configured
// are dropped (concurrency entries only once inFlight has drained to 0).
func (p *sqliteProvider) syncLimiters(out map[string]TenantRuntimeState) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Rate limiters: create/update for rps>0; drop the rest (token-state
	// loss on drop is harmless — the tenant is no longer rate-limited).
	for slug, st := range out {
		if st.RateLimitRPS <= 0 {
			continue
		}
		burst := st.RateBurst
		if burst < 1 {
			burst = 1
		}
		if lim := p.rl[slug]; lim != nil {
			lim.SetLimit(rate.Limit(st.RateLimitRPS)) // preserves accumulated tokens
			lim.SetBurst(burst)
		} else {
			p.rl[slug] = rate.NewLimiter(rate.Limit(st.RateLimitRPS), burst)
		}
	}
	for slug := range p.rl {
		if st, ok := out[slug]; !ok || st.RateLimitRPS <= 0 {
			delete(p.rl, slug)
		}
	}

	// Concurrency: create/update cap for cap>0, preserving live inFlight.
	for slug, st := range out {
		if st.ConcurrencyLimit <= 0 {
			continue
		}
		if cs := p.cc[slug]; cs != nil {
			cs.cap.Store(int64(st.ConcurrencyLimit))
		} else {
			cs := &concState{}
			cs.cap.Store(int64(st.ConcurrencyLimit))
			p.cc[slug] = cs
		}
	}
	for slug, cs := range p.cc {
		st, ok := out[slug]
		if !ok || st.ConcurrencyLimit <= 0 {
			cs.cap.Store(0) // now unlimited; existing leases drain via their decrements
			if cs.inFlight.Load() == 0 {
				delete(p.cc, slug)
			}
		}
	}
}
