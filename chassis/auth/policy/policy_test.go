package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
)

func ctxWith(caps ...string) context.Context {
	return auth.WithContext(context.Background(), &auth.Context{
		Source:       "signed",
		Capabilities: caps,
	})
}

func TestRequireCapabilityExactMatch(t *testing.T) {
	if err := RequireCapability(ctxWith("opstack:read"), "opstack:read"); err != nil {
		t.Errorf("exact match denied: %v", err)
	}
	if err := RequireCapability(ctxWith("opstack:read"), "opstack:update"); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("non-match should be denied; got %v", err)
	}
}

func TestRequireCapabilityWildcards(t *testing.T) {
	cases := []struct {
		grant string
		want  string
		ok    bool
	}{
		{"opstack:*", "opstack:read", true},
		{"opstack:*", "opstack:update", true},
		{"opstack:*", "actor:read", false},
		{"actor:*", "actor:revoke", true},
		{"admin:all", "opstack:read", true},
		{"admin:all", "actor:revoke", true},
		{"admin:all", "trace:halt", true},
		{"*", "anything:goes", true},
	}
	for _, tc := range cases {
		err := RequireCapability(ctxWith(tc.grant), tc.want)
		got := err == nil
		if got != tc.ok {
			t.Errorf("grant=%q want=%q: got ok=%v, want %v (err=%v)", tc.grant, tc.want, got, tc.ok, err)
		}
	}
}

func TestRequireCapabilityNoContext(t *testing.T) {
	if err := RequireCapability(context.Background(), "opstack:read"); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("unauth'd ctx should deny; got %v", err)
	}
}

func TestRequireCapabilityEmptyGrants(t *testing.T) {
	if err := RequireCapability(ctxWith(), "opstack:read"); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("empty grants should deny; got %v", err)
	}
}

func TestRequireCapabilityMultipleGrants(t *testing.T) {
	if err := RequireCapability(ctxWith("opstack:read", "trace:halt"), "trace:halt"); err != nil {
		t.Errorf("multi-grant exact-match denied: %v", err)
	}
	if err := RequireCapability(ctxWith("opstack:read", "actor:read"), "opstack:update"); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("multi-grant non-match should deny; got %v", err)
	}
}

func TestHasCapabilityShorthand(t *testing.T) {
	ctx := ctxWith("opstack:*")
	if !HasCapability(ctx, "opstack:read") {
		t.Error("HasCapability should return true for matching wildcard")
	}
	if HasCapability(ctx, "actor:read") {
		t.Error("HasCapability should return false for non-matching")
	}
}

// TestRequireCapabilitySuperAdminShortcut — an actor flagged super_admin
// passes every check regardless of their Capabilities list. This is
// how the bootstrap admin keeps working post-tenant-rollout: phase 3's
// tenant middleware would otherwise empty out their caps for any
// non-membership tenant.
func TestRequireCapabilitySuperAdminShortcut(t *testing.T) {
	ctx := auth.WithContext(context.Background(), &auth.Context{
		Source:       "signed",
		SuperAdmin:   true,
		Capabilities: nil, // empty on purpose
	})
	if err := RequireCapability(ctx, "opstack:update"); err != nil {
		t.Errorf("super_admin with empty caps should pass; got %v", err)
	}
	if err := RequireCapability(ctx, "actor:revoke"); err != nil {
		t.Errorf("super_admin with empty caps should pass; got %v", err)
	}
}

// TestRequireSuperAdminSignedOnly — signed callers must carry the
// super_admin flag to pass; signed actors without it are denied even
// if they have admin:all in some tenant. Basic-auth and open get a
// free pass — they're the operator's emergency credential.
func TestRequireSuperAdminSignedOnly(t *testing.T) {
	// Super admin (signed) → pass
	ctx := auth.WithContext(context.Background(), &auth.Context{
		Source: "signed", SuperAdmin: true,
	})
	if err := RequireSuperAdmin(ctx); err != nil {
		t.Errorf("super_admin should pass; got %v", err)
	}

	// Signed non-super, even with admin:all in caps → DENY (this is
	// the whole point of super_admin vs tenant-scoped admin)
	ctx = auth.WithContext(context.Background(), &auth.Context{
		Source: "signed", Capabilities: []string{"admin:all"},
	})
	if err := RequireSuperAdmin(ctx); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("tenant-admin should NOT pass RequireSuperAdmin; got %v", err)
	}

	// Basic-auth → pass (operator emergency channel)
	ctx = auth.WithContext(context.Background(), &auth.Context{
		Source: "basic", Capabilities: []string{"admin:all"},
	})
	if err := RequireSuperAdmin(ctx); err != nil {
		t.Errorf("basic-auth should pass RequireSuperAdmin; got %v", err)
	}

	// Open dev mode → pass
	ctx = auth.WithContext(context.Background(), &auth.Context{
		Source: "open", Capabilities: []string{"admin:all"},
	})
	if err := RequireSuperAdmin(ctx); err != nil {
		t.Errorf("open-mode should pass RequireSuperAdmin; got %v", err)
	}

	// Browser session WITHOUT the super_admin flag (a tenant member's
	// session, even one snapshotting admin:all) → DENY. Regression guard:
	// before the fix, any browser session passed RequireSuperAdmin,
	// letting a tenant member reach operator-only endpoints (tenant
	// create, global DNS config).
	ctx = auth.WithContext(context.Background(), &auth.Context{
		Source: "browser", Capabilities: []string{"admin:all"},
	})
	if err := RequireSuperAdmin(ctx); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("browser member session must NOT pass RequireSuperAdmin; got %v", err)
	}

	// Browser session carrying the real super_admin flag (snapshotted at
	// bootstrap) → pass.
	ctx = auth.WithContext(context.Background(), &auth.Context{
		Source: "browser", SuperAdmin: true,
	})
	if err := RequireSuperAdmin(ctx); err != nil {
		t.Errorf("super_admin browser session should pass; got %v", err)
	}

	// No context → deny
	if err := RequireSuperAdmin(context.Background()); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("missing context should deny; got %v", err)
	}
}
