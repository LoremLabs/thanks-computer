package auth

import "os"

// ResolveTenant picks the tenant slug for an outbound admin request.
// Precedence (highest first):
//
//  1. explicit --tenant flag value
//  2. TXCO_TENANT environment variable
//  3. the active profile's Meta.DefaultTenant
//  4. the literal "default" tenant (seeded by migration 0008)
//
// An empty value at any rung falls through to the next; the bottom
// rung is never empty so the CLI never silently regresses to the
// legacy flat routes once phase 4 retires them.
//
// profileFlag is the same --profile value used by signer resolution,
// so the "active profile's default" rung consults the same identity
// as the outbound signature. Errors loading the meta are non-fatal:
// the helper just continues to the next rung.
//
// Lives in the auth subpackage because it depends on ResolveProfile,
// MetaPath, and LoadMeta — none of which the upper cli package can
// import without a cycle. Upper-package call sites use this helper
// via auth.ResolveTenant.
func ResolveTenant(flag, profileFlag string) string {
	if flag != "" {
		return flag
	}
	if v := os.Getenv("TXCO_TENANT"); v != "" {
		return v
	}
	if name, err := ResolveProfile(profileFlag); err == nil && name != "" && name != ActiveNone {
		if metaPath, err := MetaPath(name); err == nil {
			if m, err := LoadMeta(metaPath); err == nil && m.DefaultTenant != "" {
				return m.DefaultTenant
			}
		}
	}
	return DefaultTenantSlug
}

// DefaultTenantSlug is the slug that migration 0008 seeds and that
// ResolveTenant falls back to. Exported so other callers (e.g. server
// tests) can reference the same constant without hard-coding the
// string.
const DefaultTenantSlug = "default"
