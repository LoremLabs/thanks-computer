package authntest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// service makes a product principal that exists (it has a credential), which
// is what a grant needs of it, and returns it managed by stack.
func service(t testing.TB, s *authn.Store, tenantID, stack, name string) authn.Principal {
	t.Helper()
	p, err := authn.ParsePrincipal("service:" + name)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueCredential(context.Background(), tenantID, stack, p, authn.NewCredential{Scopes: []string{"drive:*:*"}}); err != nil {
		t.Fatalf("make %s: %v", p.ID, err)
	}
	return p
}

func secret(name string) authn.Resource {
	return authn.Resource{Kind: authn.ResourceSecret, Name: name}
}

func capability(name string) authn.Resource {
	return authn.Resource{Kind: authn.ResourceCapability, Name: name}
}

func grantCases(t *testing.T, newStore func(t *testing.T) *authn.Store) {
	ctx := context.Background()

	t.Run("put is idempotent and Granted reads it", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")

		g, created, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: " CRM_KEY "})
		if err != nil || !created {
			t.Fatalf("put: created=%v err=%v", created, err)
		}
		if !strings.HasPrefix(g.ID, "grt_") || g.Principal != p || g.Resource != secret("CRM_KEY") ||
			strings.Join(g.Verbs, ",") != "release" || g.CreatedBy != "web" || g.Revoked() {
			t.Errorf("grant = %+v", g)
		}
		// A retried pipeline, and the stack's other slots, get the same row.
		for _, stack := range []string{"web", "web/canary", "web/_mail"} {
			again, created, err := s.PutGrant(ctx, tn, stack, p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY", Verbs: []string{"release", "release"}})
			if err != nil || created || again.ID != g.ID {
				t.Errorf("stack %q: again=%+v created=%v err=%v", stack, again, created, err)
			}
		}
		c, created, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceCapability, Name: "crm.lookup"})
		if err != nil || !created || c.Resource != capability("crm.lookup") || strings.Join(c.Verbs, ",") != "invoke" {
			t.Fatalf("capability put: %+v created=%v err=%v", c, created, err)
		}

		for _, tc := range []struct {
			what string
			res  authn.Resource
			verb string
			want bool
		}{
			{"the secret", secret("CRM_KEY"), authn.VerbRelease, true},
			{"the capability", capability("crm.lookup"), authn.VerbInvoke, true},
			{"a verb the grant does not name", secret("CRM_KEY"), authn.VerbInvoke, false},
			{"another secret", secret("DB_DSN"), authn.VerbRelease, false},
			{"names are compared as written", secret("crm_key"), authn.VerbRelease, false},
			{"a secret of the capability's name", secret("crm"), authn.VerbRelease, false},
			{"a family is not a grant", capability("crm"), authn.VerbInvoke, false},
		} {
			got, err := s.Granted(ctx, tn, p, tc.res, tc.verb)
			if err != nil || got != tc.want {
				t.Errorf("%s: granted=%v err=%v, want %v", tc.what, got, err, tc.want)
			}
		}
		other := service(t, s, tn, "web", "billing")
		if got, _ := s.Granted(ctx, tn, other, secret("CRM_KEY"), authn.VerbRelease); got {
			t.Error("another principal holds the grant")
		}
		if got, _ := s.Granted(ctx, tenant(), p, secret("CRM_KEY"), authn.VerbRelease); got {
			t.Error("the grant holds in another tenant")
		}
		if got, err := s.Granted(ctx, tn, authn.Principal{}, secret("CRM_KEY"), authn.VerbRelease); got || err != nil {
			t.Errorf("no one: granted=%v err=%v", got, err)
		}
	})

	t.Run("only the stack that manages a principal grants to it", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		in := authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY"}

		var oe *authn.OwnerError
		if _, _, err := s.PutGrant(ctx, tn, "ingest", p, in); !errors.As(err, &oe) || oe.Owner != "web" {
			t.Errorf("put from another stack: %v", err)
		}
		if _, _, err := s.PutGrant(ctx, tn, "", p, in); !errors.Is(err, authn.ErrNoStack) {
			t.Errorf("put with no stack: %v", err)
		}
		// A principal nobody has written is not there to be granted anything:
		// another stack could claim the name later.
		ghost, _ := authn.ParsePrincipal("service:ghost")
		if _, _, err := s.PutGrant(ctx, tn, "web", ghost, in); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("put to an unwritten principal: %v", err)
		}
		if _, _, err := s.PutGrant(ctx, tn, "web", authn.UserPrincipal("usr_22222222"), in); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("put to a missing user: %v", err)
		}
		if got, _ := s.Granted(ctx, tn, ghost, secret("CRM_KEY"), authn.VerbRelease); got {
			t.Error("a refused put granted something")
		}

		g, _, err := s.PutGrant(ctx, tn, "web", p, in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListGrants(ctx, tn, "ingest", p, "", false); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("list from another stack: %v", err)
		}
		if _, _, err := s.RevokeGrant(ctx, tn, "ingest", g.ID); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("revoke from another stack: %v", err)
		}
		if _, _, err := s.RevokeGrantOn(ctx, tn, "ingest", p, authn.ResourceSecret, "CRM_KEY"); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("revoke by name from another stack: %v", err)
		}
		if got, _ := s.Granted(ctx, tn, p, secret("CRM_KEY"), authn.VerbRelease); !got {
			t.Error("a refused revoke touched the grant")
		}
		// Reading by resource is tenant-wide, and says who to ask.
		on, err := s.GrantsOn(ctx, tn, authn.ResourceSecret, "CRM_KEY")
		if err != nil || len(on) != 1 || on[0].ID != g.ID || on[0].CreatedBy != "web" {
			t.Errorf("grants on the secret: %+v err=%v", on, err)
		}
	})

	t.Run("grants are tenant-scoped", func(t *testing.T) {
		s, t1, t2 := newStore(t), tenant(), tenant()
		p1 := service(t, s, t1, "web", "research")
		service(t, s, t2, "web", "research")
		g, _, err := s.PutGrant(ctx, t1, "web", p1, authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.RevokeGrant(ctx, t2, "web", g.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant revoke: %v", err)
		}
		if on, err := s.GrantsOn(ctx, t2, authn.ResourceSecret, "CRM_KEY"); err != nil || len(on) != 0 {
			t.Errorf("cross-tenant read: %+v err=%v", on, err)
		}
		if list, err := s.ListGrants(ctx, t2, "web", p1, "", true); err != nil || len(list) != 0 {
			t.Errorf("cross-tenant list: %+v err=%v", list, err)
		}
	})

	t.Run("list and revoke", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		a, _, _ := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "A_KEY"})
		b, _, _ := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceCapability, Name: "mail.send"})
		c, _, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "C_KEY"})
		if err != nil {
			t.Fatal(err)
		}
		list, err := s.ListGrants(ctx, tn, "web", p, "", false)
		if err != nil || len(list) != 3 || list[0].ID != c.ID || list[1].ID != b.ID || list[2].ID != a.ID {
			t.Fatalf("list (newest first): %+v err=%v", list, err)
		}
		if only, err := s.ListGrants(ctx, tn, "web", p, authn.ResourceSecret, false); err != nil || len(only) != 2 {
			t.Errorf("list of one kind: %+v err=%v", only, err)
		}
		if _, err := s.ListGrants(ctx, tn, "web", p, "drive", false); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("list of an unknown kind: %v", err)
		}

		got, revoked, err := s.RevokeGrant(ctx, tn, "web/canary", a.ID)
		if err != nil || !revoked || !got.Revoked() {
			t.Fatalf("revoke: %+v revoked=%v err=%v", got, revoked, err)
		}
		if _, again, err := s.RevokeGrant(ctx, tn, "web", a.ID); err != nil || again {
			t.Errorf("second revoke: revoked=%v err=%v", again, err)
		}
		if ok, _ := s.Granted(ctx, tn, p, secret("A_KEY"), authn.VerbRelease); ok {
			t.Error("a revoked grant still holds")
		}
		if _, _, err := s.RevokeGrant(ctx, tn, "web", "grt_missing"); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("revoke a missing grant: %v", err)
		}

		// By name: the grant as it was put.
		if got, revoked, err := s.RevokeGrantOn(ctx, tn, "web", p, authn.ResourceCapability, "mail.send"); err != nil || !revoked || got.ID != b.ID {
			t.Errorf("revoke by name: %+v revoked=%v err=%v", got, revoked, err)
		}
		if _, revoked, err := s.RevokeGrantOn(ctx, tn, "web", p, authn.ResourceCapability, "mail.send"); err != nil || revoked {
			t.Errorf("second revoke by name: revoked=%v err=%v", revoked, err)
		}
		if _, _, err := s.RevokeGrantOn(ctx, tn, "web", p, authn.ResourceCapability, "never.granted"); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("revoke a grant never given: %v", err)
		}

		// Pinned to a principal, an id reaches only that principal's grants.
		other := service(t, s, tn, "web", "billing")
		og, _, _ := s.PutGrant(ctx, tn, "web", other, authn.NewGrant{Kind: authn.ResourceSecret, Name: "C_KEY"})
		if _, _, err := s.RevokeGrantOf(ctx, tn, "web", p, og.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("RevokeGrantOf another principal's id: %v", err)
		}
		if ok, _ := s.Granted(ctx, tn, other, secret("C_KEY"), authn.VerbRelease); !ok {
			t.Error("a refused pinned revoke touched the grant")
		}
		if _, revoked, err := s.RevokeGrantOf(ctx, tn, "web", p, c.ID); err != nil || !revoked {
			t.Errorf("RevokeGrantOf its own: revoked=%v err=%v", revoked, err)
		}

		if live, _ := s.ListGrants(ctx, tn, "web", p, "", false); len(live) != 0 {
			t.Errorf("live after revoking all: %+v", live)
		}
		if all, _ := s.ListGrants(ctx, tn, "web", p, "", true); len(all) != 3 {
			t.Errorf("full list: %d rows", len(all))
		}
		// A revoked grant can be given again; it is a new row.
		again, created, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "A_KEY"})
		if err != nil || !created || again.ID == a.ID {
			t.Errorf("put after revoke: %+v created=%v err=%v", again, created, err)
		}
	})

	t.Run("a disabled user holds nothing, and everything again once enabled", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		u, _, _, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "alice@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.PutGrant(ctx, tn, "web", u.Principal(), authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY"}); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.Granted(ctx, tn, u.Principal(), secret("CRM_KEY"), authn.VerbRelease); !ok {
			t.Fatal("the user's grant does not hold")
		}
		_, _ = s.SetUserDisabled(ctx, tn, "web", u.ID, true)
		if ok, err := s.Granted(ctx, tn, u.Principal(), secret("CRM_KEY"), authn.VerbRelease); ok || err != nil {
			t.Errorf("disabled: granted=%v err=%v", ok, err)
		}
		if _, _, err := s.PutGrant(ctx, tn, "web", u.Principal(), authn.NewGrant{Kind: authn.ResourceSecret, Name: "DB_DSN"}); !errors.Is(err, authn.ErrDisabled) {
			t.Errorf("put to a disabled user: %v", err)
		}
		// Nothing was revoked.
		if list, _ := s.ListGrants(ctx, tn, "web", u.Principal(), "", false); len(list) != 1 {
			t.Errorf("disable revoked a grant: %+v", list)
		}
		_, _ = s.SetUserDisabled(ctx, tn, "web", u.ID, false)
		if ok, _ := s.Granted(ctx, tn, u.Principal(), secret("CRM_KEY"), authn.VerbRelease); !ok {
			t.Error("enabling the user did not restore the grant")
		}
	})

	t.Run("grant arguments", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		for name, in := range map[string]authn.NewGrant{
			"no kind":                  {Name: "CRM_KEY"},
			"unknown kind":             {Kind: "drive", Name: "dc_1"},
			"no name":                  {Kind: authn.ResourceSecret},
			"secret with a dot":        {Kind: authn.ResourceSecret, Name: "crm.key"},
			"secret starting on digit": {Kind: authn.ResourceSecret, Name: "1KEY"},
			"secret too long":          {Kind: authn.ResourceSecret, Name: strings.Repeat("K", 129)},
			"capability in capitals":   {Kind: authn.ResourceCapability, Name: "CRM.lookup"},
			"capability family":        {Kind: authn.ResourceCapability, Name: "crm.*"},
			"capability empty segment": {Kind: authn.ResourceCapability, Name: "crm..lookup"},
			"capability with a colon":  {Kind: authn.ResourceCapability, Name: "secret:CRM_KEY"},
			"wildcard":                 {Kind: authn.ResourceSecret, Name: "*"},
			"verb of another kind":     {Kind: authn.ResourceSecret, Name: "CRM_KEY", Verbs: []string{"invoke"}},
			"unknown verb":             {Kind: authn.ResourceCapability, Name: "crm.lookup", Verbs: []string{"invoke", "admin"}},
			"wildcard verb":            {Kind: authn.ResourceCapability, Name: "crm.lookup", Verbs: []string{"*"}},
		} {
			if _, _, err := s.PutGrant(ctx, tn, "web", p, in); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, _, err := s.PutGrant(ctx, "", "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY"}); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("no tenant: %v", err)
		}
		if list, _ := s.ListGrants(ctx, tn, "web", p, "", true); len(list) != 0 {
			t.Errorf("a refused put wrote a row: %+v", list)
		}
	})

	t.Run("resource names round-trip through their string form", func(t *testing.T) {
		for in, want := range map[string]authn.Resource{
			"crm.lookup":            capability("crm.lookup"),
			"capability:crm.lookup": capability("crm.lookup"),
			"secret:CRM_KEY":        secret("CRM_KEY"),
			" secret:CRM_KEY ":      secret("CRM_KEY"),
		} {
			got, err := authn.ParseResource(in)
			if err != nil || got != want {
				t.Errorf("parse %q: %+v err=%v", in, got, err)
			}
			if again, err := authn.ParseResource(got.String()); err != nil || again != got {
				t.Errorf("%q does not round-trip: %q → %+v err=%v", in, got.String(), again, err)
			}
		}
		for _, bad := range []string{"", "secret:", ":CRM_KEY", "drive:dc_1", "CRM_KEY", "secret:a:b", "crm.*"} {
			if got, err := authn.ParseResource(bad); err == nil {
				t.Errorf("parse %q: accepted as %+v", bad, got)
			}
		}
	})

	t.Run("the index allows one live grant per principal and resource", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		if _, _, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "CRM_KEY"}); err != nil {
			t.Fatal(err)
		}
		insert := s.Dialect.Rebind(`INSERT INTO resource_grants
			(id, tenant_id, principal_id, resource_kind, resource_id, verbs, created_by, created_at, revoked_at)
			VALUES (?, ?, ?, 'secret', 'CRM_KEY', '["release"]', 'web', '2026-09-29T00:00:00Z', ?)`)
		_, err := s.DB.ExecContext(ctx, insert, "grt_dupe_live"+tn, tn, p.ID, nil)
		if !s.Dialect.IsUniqueViolationGeneric(err) {
			t.Errorf("a second live grant was accepted: %v", err)
		}
		if _, err := s.DB.ExecContext(ctx, insert, "grt_dupe_revoked"+tn, tn, p.ID, "2026-09-29T00:00:01Z"); err != nil {
			t.Errorf("a revoked duplicate was refused: %v", err)
		}
	})

	t.Run("a corrupt row is an error, never a grant", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		for i, verbs := range []string{`[]`, `"release"`, `not json`, ``} {
			name := "BAD_" + string(rune('A'+i))
			if _, err := s.DB.ExecContext(ctx, s.Dialect.Rebind(`INSERT INTO resource_grants
				(id, tenant_id, principal_id, resource_kind, resource_id, verbs, created_by, created_at)
				VALUES (?, ?, ?, 'secret', ?, ?, 'web', '2026-09-29T00:00:00Z')`),
				"grt_bad"+hxid.NewTimeSort().String(), tn, p.ID, name, verbs); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.Granted(ctx, tn, p, secret(name), authn.VerbRelease); ok || err == nil {
				t.Errorf("verbs %q: granted=%v err=%v", verbs, ok, err)
			}
		}
	})

	t.Run("the live cap", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := service(t, s, tn, "web", "research")
		insert := s.Dialect.Rebind(`INSERT INTO resource_grants
			(id, tenant_id, principal_id, resource_kind, resource_id, verbs, created_by, created_at)
			VALUES (?, ?, ?, 'secret', ?, '["release"]', 'web', '2026-09-29T00:00:00Z')`)
		for i := 0; i < authn.MaxGrants; i++ {
			id := hxid.NewTimeSort().String()
			if _, err := s.DB.ExecContext(ctx, insert, "grt_"+id, tn, p.ID, "FILL_"+id); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.PutGrant(ctx, tn, "web", p, authn.NewGrant{Kind: authn.ResourceSecret, Name: "ONE_MORE"}); !errors.Is(err, authn.ErrTooManyGrants) {
			t.Errorf("put past the cap: %v", err)
		}
	})
}
