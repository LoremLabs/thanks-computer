package policy

// Capability strings follow Apache Shiro's shape: domain:instance:action.
// Wildcards (`*`) are allowed at any segment. v1 always uses `*` for
// the instance segment — there's no per-resource scoping yet — but
// the structure stays so adding `opstack:<id>:update` later is a
// small extension rather than a model rewrite.
//
// Two aliases are kept for muscle memory and back-compat with rows
// already stored by the v1 invitation flow:
//
//   - "admin:all" ⇄ "*:*:*"
//   - bare "*"    ⇄ "*:*:*"
//
// The matcher in policy.go reads these aliases natively; this file
// owns the canonical form (used at write time) and the whitelist of
// known strings (a write-time gate that catches typos like
// "opstack:reed" before they're persisted as a no-op role).

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// KnownCapabilities is the canonical v2 whitelist. The matcher does
// not consult this map — it accepts any well-formed 3-segment string
// and wildcards. The whitelist is a write-time guard: ParseCapabilities
// and the server's invite + grant-member handlers refuse anything
// outside it so users don't end up with non-functional roles from a
// typo.
//
// Adding per-instance grants later means relaxing the validator to
// accept "<known-domain>:<id-or-*>:<known-action>", not editing this
// list per resource.
var KnownCapabilities = map[string]bool{
	// Aliases for chassis-wide admin
	"admin:all": true,
	"*":         true,
	"*:*:*":     true,

	// Operations / rules
	"opstack:*:read":     true,
	"opstack:*:update":   true,
	"opstack:*:activate": true,
	"opstack:*:*":        true,

	// Actor / membership management
	"actor:*:read":   true,
	"actor:*:invite": true,
	"actor:*:revoke": true,
	"actor:*:*":      true,

	// Hostname routing (claim, revoke, challenge, verify all gated
	// by this single capability — they're all "manipulate the
	// tenant_hostnames table for this tenant").
	"hostname:*:read":  true,
	"hostname:*:write": true,
	"hostname:*:*":     true,

	// Per-tenant secret store. `read` lists secrets and reads
	// metadata (NEVER the value). `write` covers create, generate,
	// rotate, rotate-generated, patch-description, and revoke.
	// There is NO `secret:*:reveal` in v1 — per design §5, revealing
	// is itself a leak surface; to inspect a value, rotate it.
	"secret:*:read":  true,
	"secret:*:write": true,
	"secret:*:*":     true,

	// Authoritative-DNS zones + records (delegated zones, override
	// records, and the zone-render preview — all "manipulate this
	// tenant's DNS authority"). `read` lists/renders; `write` creates
	// and revokes zones + override records.
	"dns:*:read":  true,
	"dns:*:write": true,
	"dns:*:*":     true,
}

// ErrUnknownCapability is returned by ValidateCapabilities and
// ParseCapabilities for any string outside KnownCapabilities. The
// offending value is wrapped via fmt.Errorf so callers can render it
// to users; an errors.Is check against this sentinel still works.
var ErrUnknownCapability = errors.New("unknown_capability")

// Canonical returns the storage form of a capability string. The
// matcher tolerates either shape; we canonicalize on write so the
// database stays consistent and `txco auth whoami` doesn't render
// the same role two different ways depending on history.
//
//	"admin:all"      → "*:*:*"
//	"*"              → "*:*:*"
//	"opstack:read"   → "opstack:*:read"   (2-seg legacy normalisation)
//	"opstack:*:read" → unchanged
//
// Returns "" for empty input. Returns the original (unchanged) for
// anything with 4+ colons — those fail validation downstream.
func Canonical(cap string) string {
	cap = strings.TrimSpace(cap)
	if cap == "" {
		return ""
	}
	if cap == "admin:all" || cap == "*" {
		return "*:*:*"
	}
	segs := strings.Split(cap, ":")
	switch len(segs) {
	case 2:
		// 2-segment legacy: insert a `*` instance between domain and
		// action. So "opstack:read" → "opstack:*:read".
		return segs[0] + ":*:" + segs[1]
	case 3:
		return cap
	default:
		return cap // leave the malformed string for the validator
	}
}

// ValidateCapabilities returns a wrapped ErrUnknownCapability the
// first time it sees a string outside KnownCapabilities. Inputs are
// canonicalized before lookup so "admin:all" and "*:*:*" both pass.
//
// An empty slice is valid — callers (the invite handler) supply
// the back-compat default elsewhere when no caps are sent.
func ValidateCapabilities(caps []string) error {
	for _, raw := range caps {
		c := Canonical(raw)
		if c == "" {
			return fmt.Errorf("%w: empty capability string", ErrUnknownCapability)
		}
		if !KnownCapabilities[c] {
			return fmt.Errorf("%w: %q", ErrUnknownCapability, raw)
		}
	}
	return nil
}

// Covers reports whether `want` is matched by any string in `grants`.
// Same wildcard rules as RequireCapability (3-segment, segment-by-
// segment), used at write time by the anti-privilege-escalation
// guards on /auth/invitations and /auth/members so a granter can't
// hand out capabilities they don't have themselves.
func Covers(grants []string, want string) bool {
	wantSegs := segments(want)
	for _, g := range grants {
		if matches(g, want, wantSegs) {
			return true
		}
	}
	return false
}

// CoversAll returns the first `want` capability not covered by
// `grants`, or "" if all are covered. The empty-string sentinel lets
// callers branch on (`if missing := CoversAll(...); missing != ""`)
// instead of allocating an error in the happy path. The caller
// formats the surface error (HTTP 403 + denied_capability body).
func CoversAll(grants, wants []string) string {
	for _, w := range wants {
		if !Covers(grants, w) {
			return w
		}
	}
	return ""
}

// ParseCapabilities is the user-facing entrypoint used by the CLI's
// `--caps` flag. It splits on commas, trims whitespace, dedupes,
// canonicalizes each entry, and validates against the whitelist.
// Empty input returns (nil, nil) so callers can apply their own
// default (e.g. `["admin:all"]` for the invitation flow).
//
// Errors are wrapped with %w so callers can errors.Is against
// ErrUnknownCapability for typed handling.
func ParseCapabilities(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range strings.Split(s, ",") {
		c := Canonical(raw)
		if c == "" {
			continue
		}
		if !KnownCapabilities[c] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownCapability, strings.TrimSpace(raw))
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	// Sort for deterministic storage: `["actor:*:read","opstack:*:*"]`
	// and `["opstack:*:*","actor:*:read"]` should yield the same
	// canonical JSON so equality checks and diffs stay stable.
	sort.Strings(out)
	return out, nil
}
