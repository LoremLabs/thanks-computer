// Package admission holds the chassis's pre-pipeline admission gates:
// a per-tenant runtime-state check (a suspended or disabled tenant is
// denied before its stack runs), per-tenant node-local rate-limit and
// concurrency caps, and a process-level drain flag (signal-driven; bleeds
// the node out of a load balancer). All are open-core mechanism and
// unopinionated: a tenant with no state row (or 0 limits) is admitted, and
// drain is off until signaled, so a single-node chassis behaves exactly as
// it did before this package existed. The commercial policy that decides
// WHO is suspended/limited lives outside the chassis and arrives as
// tenant_runtime_state rows (an operator edit or a fleet-sync
// entitlement.updated apply).
package admission

import "time"

// TenantRuntimeState is the operational admission state for one tenant,
// mirrored from a tenant_runtime_state row.
type TenantRuntimeState struct {
	Enabled    bool
	Suspended  bool
	DenyStatus int    // HTTP status to render when denied; 0 => default 403
	DenyReason string // short machine token surfaced as a response header

	// Phase 2 — node-local operational limits. 0 => unlimited.
	RateLimitRPS     float64 // resolved per-second rate (may be fractional, e.g. 50/min => 0.833)
	RateBurst        int     // token-bucket size (max burst); defaults to ceil(2*rps) upstream
	ConcurrencyLimit int     // max simultaneous in-flight requests
}

// Admitted reports whether a tenant in this state may enter its stack.
// Either knob denies: a disabled OR a suspended tenant is refused. (Rate
// and concurrency are enforced separately — see AllowRate/AcquireConcurrency.)
func (s TenantRuntimeState) Admitted() bool { return s.Enabled && !s.Suspended }

// Decision is the result of an admission check.
type Decision struct {
	Admit  bool
	Status int           // HTTP status when !Admit (402 | 403 | 429 | ...)
	Reason string        // machine token for the response header
	Retry  time.Duration // suggested Retry-After (0 => none); rendered by the outlet
}
