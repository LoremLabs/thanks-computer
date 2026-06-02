// Package admission holds the chassis's pre-pipeline admission gates:
// a per-tenant runtime-state check (a suspended or disabled tenant is
// denied before its stack runs) and a process-level drain flag
// (signal-driven; bleeds the node out of a load balancer). Both are
// open-core mechanism and unopinionated: a tenant with no state row is
// admitted, and drain is off until signaled, so a single-node chassis
// behaves exactly as it did before this package existed. The commercial
// policy that decides WHO is suspended lives outside the chassis and
// arrives as tenant_runtime_state rows (an operator edit or a fleet-sync
// entitlement.updated apply).
package admission

// TenantRuntimeState is the operational admission state for one tenant,
// mirrored from a tenant_runtime_state row. Phase 1 reads only the
// admission fields; the rate/concurrency columns are reserved for Phase 2.
type TenantRuntimeState struct {
	Enabled    bool
	Suspended  bool
	DenyStatus int    // HTTP status to render when denied; 0 => default 403
	DenyReason string // short machine token surfaced as a response header
}

// Admitted reports whether a tenant in this state may enter its stack.
// Either knob denies: a disabled OR a suspended tenant is refused.
func (s TenantRuntimeState) Admitted() bool { return s.Enabled && !s.Suspended }

// Decision is the result of an admission check.
type Decision struct {
	Admit  bool
	Status int    // HTTP status when !Admit (402 | 403 | ...)
	Reason string // machine token for the response header
}
