// Package policy gates handlers behind capability checks. Capabilities
// follow Apache Shiro's `domain:instance:action` shape with `*`
// wildcards at any segment:
//
//   opstack:*:read     — read any opstack
//   opstack:abc:read   — read specifically opstack "abc" (future)
//   opstack:*:*        — any action on any opstack
//   *:*:*              — chassis-wide admin
//
// Two aliases stay valid for muscle memory and back-compat:
//   "admin:all" ⇄ "*:*:*"
//   "*"         ⇄ "*:*:*"
//
// v1 doesn't issue per-instance checks — every want/grant uses `*`
// in the instance slot — but the matcher already supports them so
// adding per-resource scoping later is a one-line handler change.
//
// Tenant scoping is enforced at the URL prefix (`/v1/tenants/{t}/…`)
// in the admin mux: the tenant resolver loads the caller's membership
// for that tenant and replaces auth.Context.Capabilities with the
// scoped set BEFORE the handler runs. So RequireCapability itself
// stays a pure list-match — what changes between routes is which list
// it sees on the context.
//
// Two short-circuits live here:
//   - SuperAdmin (signed actor with actors.super_admin = 1): allow
//     every RequireCapability call regardless of memberships.
//   - basic-auth / open callers (Source != "signed"): the auth
//     middleware already synthesized admin:all for them, so they pass
//     the list match. Treating them as super-admin too in
//     RequireSuperAdmin keeps operator-emergency access intact.
package policy

import (
	"context"
	"errors"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth"
)

// ErrCapabilityDenied is returned by RequireCapability when none of the
// caller's granted capabilities match the requested one. Middleware
// maps this to HTTP 403 with the standard error code.
var ErrCapabilityDenied = errors.New("capability_denied")

// RequireCapability checks the auth context attached to ctx and returns
// nil iff the caller holds a capability matching `want`. If the context
// is unset (a path that bypasses auth like /healthz) or no granted
// capability matches, returns ErrCapabilityDenied.
//
// `want` is a 3-segment capability string (`domain:instance:action`).
// 2-segment legacy strings (`opstack:read`) are normalised at compare
// time, so a stale call site won't silently miss its grant. Granted
// capabilities are compared segment-by-segment, with `*` matching
// anything at that position. The aliases `admin:all` and bare `*`
// expand to `*:*:*`.
func RequireCapability(ctx context.Context, want string) error {
	c := auth.FromContext(ctx)
	if c == nil {
		return ErrCapabilityDenied
	}
	if c.SuperAdmin {
		return nil
	}
	wantSegs := segments(want)
	for _, granted := range c.Capabilities {
		if matches(granted, want, wantSegs) {
			return nil
		}
	}
	return ErrCapabilityDenied
}

// RequireSuperAdmin gates a chassis-wide endpoint behind the
// super_admin flag (or a non-signed caller, which is treated as an
// operator with chassis access). Used for tenant CRUD — creating /
// destroying tenants is a chassis-wide action that no per-tenant
// membership should be able to perform.
func RequireSuperAdmin(ctx context.Context) error {
	c := auth.FromContext(ctx)
	if c == nil {
		return ErrCapabilityDenied
	}
	if c.SuperAdmin {
		return nil
	}
	// basic-auth / open: the operator already has chassis credentials,
	// so this is the same trust level as super_admin in practice.
	if c.Source != "" && c.Source != "signed" {
		return nil
	}
	return ErrCapabilityDenied
}

// HasCapability is the non-erroring shorthand. Useful when a handler
// wants to branch on whether a caller can do something without
// outright denying the request.
func HasCapability(ctx context.Context, want string) bool {
	return RequireCapability(ctx, want) == nil
}

// matches reports whether `granted` covers `want`. Both are
// normalised to 3-segment form first, then compared segment-by-
// segment: each segment of `granted` matches if it's `*` OR string-
// equal to the corresponding segment in `want`. The aliases
// "admin:all" and "*" canonicalise to "*:*:*" — handled by segments().
//
// `wantSegs` is passed in (rather than re-computed) because the
// hot loop in RequireCapability iterates over the caller's grants;
// canonicalising the want once outside the loop saves repeated work.
func matches(granted, want string, wantSegs [3]string) bool {
	if granted == "" {
		return want == ""
	}
	g := segments(granted)
	for i := 0; i < 3; i++ {
		if g[i] == "*" {
			continue
		}
		if g[i] != wantSegs[i] {
			return false
		}
	}
	return true
}

// segments splits a capability string into exactly 3 parts. The two
// aliases ("admin:all", "*") collapse to ["*","*","*"]; a 2-segment
// legacy string ("opstack:read") gets a "*" instance inserted. Any
// other shape returns ["","",""] which never matches a non-empty
// want.
func segments(cap string) [3]string {
	cap = strings.TrimSpace(cap)
	switch cap {
	case "":
		return [3]string{}
	case "admin:all", "*":
		return [3]string{"*", "*", "*"}
	}
	parts := strings.Split(cap, ":")
	switch len(parts) {
	case 2:
		return [3]string{parts[0], "*", parts[1]}
	case 3:
		return [3]string{parts[0], parts[1], parts[2]}
	default:
		return [3]string{}
	}
}
