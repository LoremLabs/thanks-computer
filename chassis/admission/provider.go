package admission

import (
	"database/sql"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

const defaultDenyStatus = 403

// Provider answers the per-tenant admission question on the request hot
// path. Implementations must be lock-light and non-blocking — no DB or
// network I/O per request.
type Provider interface {
	// Decide returns the admission decision for a tenant slug. An unknown
	// tenant (no row) and the system tenant are always admitted.
	Decide(tenant string) Decision
}

// NopProvider admits everyone. The nil/test/disabled default so call
// sites can stay unconditional.
type NopProvider struct{}

// Decide implements Provider.
func (NopProvider) Decide(string) Decision { return Decision{Admit: true} }

// sqliteProvider serves admission decisions from an in-memory snapshot of
// tenant_runtime_state, rebuilt on every dbcache reload. The snapshot is
// held behind an atomic pointer so reads are lock-free.
type sqliteProvider struct {
	snap   atomic.Pointer[map[string]TenantRuntimeState] // key = tenant slug
	logger *zap.Logger
}

// NewSQLiteProvider returns a provider seeded with an empty snapshot, so
// reads before the first Rebuild admit everyone.
func NewSQLiteProvider(logger *zap.Logger) *sqliteProvider {
	p := &sqliteProvider{logger: logger}
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

// Rebuild replaces the snapshot from the given handle. On ANY error
// (including the table being absent before the 0014 migration applies)
// the previous snapshot is kept — admission never goes dark mid-flight,
// it keeps serving the last good state (an empty snapshot admits all).
func (p *sqliteProvider) Rebuild(db *sql.DB) error {
	if p == nil || db == nil {
		return nil
	}
	rows, err := db.Query(`
        SELECT t.slug, s.enabled, s.suspended, s.deny_status, s.deny_reason
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
		)
		if err := rows.Scan(&slug, &enabled, &suspended, &denyStatus, &denyReason); err != nil {
			if p.logger != nil {
				p.logger.Warn("admission provider rebuild: scan", zap.Error(err))
			}
			return err
		}
		out[slug] = TenantRuntimeState{
			Enabled:    enabled != 0,
			Suspended:  suspended != 0,
			DenyStatus: denyStatus,
			DenyReason: denyReason,
		}
	}
	if err := rows.Err(); err != nil {
		if p.logger != nil {
			p.logger.Warn("admission provider rebuild: rows", zap.Error(err))
		}
		return err
	}
	p.snap.Store(&out)
	if p.logger != nil {
		p.logger.Debug("admission provider rebuilt", zap.Int("tenants", len(out)))
	}
	return nil
}
