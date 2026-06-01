package auth

// Named capability roles. Capabilities are Shiro-style `domain:instance:action`
// strings (v1 uses `*` for instance until per-resource scoping lands). Granting
// by a named role keeps the capability set defined in exactly one place instead
// of being sprinkled as literal slices across handlers.

// RoleTenantOwner is the role granted to a cloud user over their own tenant on
// OAuth enrollment: scoped owner of everything inside the tenant, but NOT a
// chassis super-admin (they own their tenant, not the chassis).
const RoleTenantOwner = "tenant_owner"

// TenantOwnerCaps is the capability set for RoleTenantOwner. Returns a fresh
// slice on each call so callers can't mutate a shared backing array.
func TenantOwnerCaps() []string {
	return []string{"opstack:*:*", "stack:*:*", "hostname:*:*"}
}
