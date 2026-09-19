// Package authntest holds the identity store's behaviour suite and the
// helpers to stand a store up in a test. The suite runs on SQLite here
// (chassis/authn) and on a real Postgres in the cloud overlay, so the two
// engines are held to one contract — the same split as vector/vectortest.
package authntest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	dbschemas "github.com/loremlabs/thanks-computer/db"
)

// ApplyAuthSchema applies every embedded auth migration for the engine
// ("sqlite" or "postgres"), in order, as the boot runner would on a fresh DB.
func ApplyAuthSchema(t testing.TB, db *sql.DB, engine string) {
	t.Helper()
	root := path.Join("schema", engine, "auth")
	ents, err := fs.ReadDir(dbschemas.FS, root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	var files []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		body, err := fs.ReadFile(dbschemas.FS, path.Join(root, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
}

// NewSQLiteStore returns a Store over a fresh, fully migrated SQLite auth DB
// in the test's temp dir.
func NewSQLiteStore(t testing.TB) *authn.Store {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "auth.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ApplyAuthSchema(t, db, "sqlite")
	return authn.NewStore(db, registry.SQLite)
}

// tenant mints a tenant id no other test (or earlier run against a
// long-lived Postgres) has used, so tests need no cleanup to be isolated.
func tenant() string { return "tnt_test" + hxid.NewTimeSort().String() }

// Conformance is the store's behaviour suite. newStore returns a store over
// a migrated auth DB; it may hand back the same database every time.
func Conformance(t *testing.T, newStore func(t *testing.T) *authn.Store) {
	ctx := context.Background()

	t.Run("create user binds the email and is idempotent per stack", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		u, b, created, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: " Alice@Example.COM ", DisplayName: "Alice"})
		if err != nil || !created {
			t.Fatalf("create: created=%v err=%v", created, err)
		}
		if !strings.HasPrefix(u.ID, "usr_") || u.Status != authn.StatusActive || u.CreatedBy != "web" || u.DisplayName != "Alice" {
			t.Errorf("user = %+v", u)
		}
		if b.Subject != "alice@example.com" || b.Kind != authn.BindEmail || b.Issuer != "" || b.Verified ||
			b.Principal != u.Principal() || b.CreatedBy != "web" {
			t.Errorf("binding = %+v", b)
		}
		if _, err := authn.ParsePrincipal(u.Principal().ID); err != nil {
			t.Errorf("minted principal does not parse: %v", err)
		}

		// A retried pipeline — and the canary slot of the same stack — get
		// the same person back. The display name is left alone.
		for _, stack := range []string{"web", "web/canary", "web/_mail"} {
			again, b2, created, err := s.CreateUser(ctx, tn, stack, authn.NewUser{Email: "alice@example.com", DisplayName: "Someone Else"})
			if err != nil || created || again.ID != u.ID || again.DisplayName != "Alice" || b2.ID != b.ID {
				t.Errorf("stack %q: again=%+v created=%v err=%v", stack, again, created, err)
			}
		}
		// verified upgrades false→true and never back.
		_, b3, _, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "alice@example.com", EmailVerified: true})
		if err != nil || !b3.Verified {
			t.Errorf("verify upgrade: %+v err=%v", b3, err)
		}
		_, b4, _, _ := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "alice@example.com"})
		if !b4.Verified {
			t.Error("a later unverified create downgraded the binding")
		}

		got, gb, err := s.UserByEmail(ctx, tn, "ALICE@example.com")
		if err != nil || got.ID != u.ID || gb.ID != b.ID {
			t.Errorf("by email: %+v err=%v", got, err)
		}
		if got, err := s.GetUser(ctx, tn, u.ID); err != nil || got.ID != u.ID {
			t.Errorf("get: %+v err=%v", got, err)
		}
	})

	t.Run("another stack may read a user but not manage it", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		u, _, _, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "bob@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		var oe *authn.OwnerError
		_, _, _, err = s.CreateUser(ctx, tn, "ingest", authn.NewUser{Email: "bob@example.com"})
		if !errors.Is(err, authn.ErrNotOwner) || !errors.As(err, &oe) || oe.Owner != "web" {
			t.Errorf("create from another stack: %v", err)
		}
		if _, err := s.SetUserDisabled(ctx, tn, "ingest", u.ID, true); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("disable from another stack: %v", err)
		}
		if _, _, err := s.IssueCredential(ctx, tn, "ingest", u.Principal(), authn.NewCredential{Scopes: []string{"imap:*:login"}}); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("issue from another stack: %v", err)
		}
		if _, err := s.ListCredentials(ctx, tn, "ingest", u.Principal(), false); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("list from another stack: %v", err)
		}
		if _, _, err := s.BindPrincipal(ctx, tn, "ingest", u.Principal(), authn.NewBinding{Kind: authn.BindEmail, Subject: "bob2@example.com"}); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("bind from another stack: %v", err)
		}
		// Reading is tenant-wide, and says who to ask.
		if got, err := s.GetUser(ctx, tn, u.ID); err != nil || got.CreatedBy != "web" {
			t.Errorf("read: %+v err=%v", got, err)
		}
		// A write with no stack to attribute it to is refused outright.
		if _, _, _, err := s.CreateUser(ctx, tn, "", authn.NewUser{Email: "carol@example.com"}); !errors.Is(err, authn.ErrNoStack) {
			t.Errorf("no stack: %v", err)
		}
	})

	t.Run("users are tenant-scoped", func(t *testing.T) {
		s, t1, t2 := newStore(t), tenant(), tenant()
		u1, _, _, err := s.CreateUser(ctx, t1, "web", authn.NewUser{Email: "dana@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		u2, _, created, err := s.CreateUser(ctx, t2, "web", authn.NewUser{Email: "dana@example.com"})
		if err != nil || !created || u2.ID == u1.ID {
			t.Errorf("same address in another tenant: %+v created=%v err=%v", u2, created, err)
		}
		if _, err := s.GetUser(ctx, t2, u1.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant get: %v", err)
		}
		if _, err := s.SetUserDisabled(ctx, t2, "web", u1.ID, true); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant disable: %v", err)
		}
	})

	t.Run("invalid arguments", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		for name, in := range map[string]authn.NewUser{
			"no email":         {},
			"no domain":        {Email: "alice@"},
			"two ats":          {Email: "a@b@c"},
			"display name":     {Email: "Alice <alice@example.com>"},
			"control char":     {Email: "ok@example.com", DisplayName: "A\x00B"},
			"name too long":    {Email: "ok@example.com", DisplayName: strings.Repeat("x", 257)},
			"space in address": {Email: "al ice@example.com"},
		} {
			if _, _, _, err := s.CreateUser(ctx, tn, "web", in); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, _, _, err := s.CreateUser(ctx, "", "web", authn.NewUser{Email: "ok@example.com"}); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("no tenant: %v", err)
		}
	})

	t.Run("disable blocks new credentials and can be undone", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		u, _, _, _ := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "erin@example.com"})
		_, pw, err := s.IssueCredential(ctx, tn, "web", u.Principal(), authn.NewCredential{Scopes: []string{"imap:*:login"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.SetUserDisabled(ctx, tn, "web/canary", u.ID, true)
		if err != nil || !got.Disabled() {
			t.Fatalf("disable: %+v err=%v", got, err)
		}
		if _, _, err := s.IssueCredential(ctx, tn, "web", u.Principal(), authn.NewCredential{Scopes: []string{"imap:*:login"}}); !errors.Is(err, authn.ErrDisabled) {
			t.Errorf("issue to a disabled user: %v", err)
		}
		// Nothing was revoked: the store still verifies the password (the
		// resolver is what refuses a disabled user), and enabling restores it.
		if _, ok, _ := s.VerifyPassword(ctx, tn, u.Principal(), pw); !ok {
			t.Error("disable revoked the credential")
		}
		if got, err := s.SetUserDisabled(ctx, tn, "web", u.ID, false); err != nil || got.Disabled() {
			t.Errorf("enable: %+v err=%v", got, err)
		}
		if _, err := s.SetUserDisabled(ctx, tn, "web", "usr_11111111", true); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("disable a missing user: %v", err)
		}
	})

	t.Run("bind a product principal", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		pony, _ := authn.ParsePrincipal("pony:paris")
		b, created, err := s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "Paris@OnePony.com", Metadata: `{"head":"imap"}`})
		if err != nil || !created || b.Subject != "paris@onepony.com" || b.Principal != pony || b.CreatedBy != "web" || b.Metadata != `{"head":"imap"}` {
			t.Fatalf("bind: %+v created=%v err=%v", b, created, err)
		}
		// Same identifier, same principal: a no-op.
		b2, created, err := s.BindPrincipal(ctx, tn, "web/canary", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "paris@onepony.com", Verified: true})
		if err != nil || created || b2.ID != b.ID || !b2.Verified {
			t.Errorf("rebind: %+v created=%v err=%v", b2, created, err)
		}
		got, err := s.LookupBinding(ctx, tn, authn.BindEmail, "", "PARIS@onepony.com")
		if err != nil || got.ID != b.ID || got.Principal != pony || !got.Verified {
			t.Errorf("lookup: %+v err=%v", got, err)
		}
		// Same identifier, another principal.
		other, _ := authn.ParsePrincipal("pony:milan")
		if _, _, err := s.BindPrincipal(ctx, tn, "web", other, authn.NewBinding{Kind: authn.BindEmail, Subject: "paris@onepony.com"}); !errors.Is(err, authn.ErrBound) {
			t.Errorf("bind a taken identifier: %v", err)
		}
		// …and a user cannot be created on a pony's address.
		if _, _, _, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "paris@onepony.com"}); !errors.Is(err, authn.ErrBound) {
			t.Errorf("user on a pony's address: %v", err)
		}
		// The first row claimed the pony for `web`.
		if _, _, err := s.BindPrincipal(ctx, tn, "core", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "paris2@onepony.com"}); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("bind from another stack: %v", err)
		}
		// A user principal needs its users row.
		ghost := authn.UserPrincipal("usr_22222222")
		if _, _, err := s.BindPrincipal(ctx, tn, "web", ghost, authn.NewBinding{Kind: authn.BindEmail, Subject: "ghost@example.com"}); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("bind a missing user: %v", err)
		}
		if _, err := s.LookupBinding(ctx, tn, authn.BindEmail, "", "nobody@example.com"); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("lookup a missing binding: %v", err)
		}

		// oidc: issuer + subject, case kept.
		ob, created, err := s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindOIDC, Issuer: "https://auth.example.com", Subject: "AbC123"})
		if err != nil || !created || ob.Issuer != "https://auth.example.com" || ob.Subject != "AbC123" {
			t.Errorf("oidc bind: %+v created=%v err=%v", ob, created, err)
		}
		// The same subject under another issuer is a different identifier.
		if _, created, err := s.BindPrincipal(ctx, tn, "web", other, authn.NewBinding{Kind: authn.BindOIDC, Issuer: "https://other.example.com", Subject: "AbC123"}); err != nil || !created {
			t.Errorf("same subject, other issuer: created=%v err=%v", created, err)
		}
		for name, nb := range map[string]authn.NewBinding{
			"reserved did":      {Kind: "did", Issuer: "did:plc", Subject: "did:plc:abc"},
			"reserved atproto":  {Kind: "atproto_handle", Subject: "alice.example.com"},
			"unknown kind":      {Kind: "phone", Subject: "+15551234"},
			"email with issuer": {Kind: authn.BindEmail, Issuer: "https://x.example.com", Subject: "a@example.com"},
			"oidc http issuer":  {Kind: authn.BindOIDC, Issuer: "http://auth.example.com", Subject: "1"},
			"oidc no subject":   {Kind: authn.BindOIDC, Issuer: "https://auth.example.com"},
			"metadata array":    {Kind: authn.BindEmail, Subject: "m@example.com", Metadata: `[1]`},
		} {
			if _, _, err := s.BindPrincipal(ctx, tn, "web", pony, nb); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the index allows one live binding per identifier", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		pony, _ := authn.ParsePrincipal("pony:rome")
		if _, _, err := s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "rome@onepony.com"}); err != nil {
			t.Fatal(err)
		}
		// Bypass the store's pre-check: issuer is NULL for email, and a plain
		// UNIQUE would treat two NULLs as distinct. The expression index must not.
		insert := s.Dialect.Rebind(`INSERT INTO principal_bindings
			(id, tenant_id, principal_id, kind, issuer, subject, verified, created_by, created_at, revoked_at)
			VALUES (?, ?, 'pony:dupe', 'email', NULL, 'rome@onepony.com', 0, 'web', '2026-09-18T00:00:00Z', ?)`)
		_, err := s.DB.ExecContext(ctx, insert, "bnd_dupe_live"+tn, tn, nil)
		if !s.Dialect.IsUniqueViolationGeneric(err) {
			t.Errorf("a second live binding was accepted: %v", err)
		}
		// A revoked row does not hold the identifier.
		if _, err := s.DB.ExecContext(ctx, insert, "bnd_dupe_revoked"+tn, tn, "2026-09-18T00:00:01Z"); err != nil {
			t.Errorf("a revoked duplicate was refused: %v", err)
		}
	})

	t.Run("issue, verify, list and revoke a credential", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		pony, _ := authn.ParsePrincipal("pony:oslo")
		c, pw, err := s.IssueCredential(ctx, tn, "web", pony, authn.NewCredential{
			Scopes: []string{"imap:*:login", "drive:dc_abc:*", "imap:*:login"}, Label: " Laptop "})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(c.ID, "crd_") || c.Principal != pony || c.Kind != authn.KindAppPassword ||
			c.Label != "Laptop" || c.CreatedBy != "web" || c.Revoked() {
			t.Errorf("credential = %+v", c)
		}
		if got := strings.Join(c.Scopes.Strings(), " "); got != "drive:dc_abc:* imap:*:login" {
			t.Errorf("scopes = %q", got)
		}
		// txc_<short-id>_<secret>, and the id in it is the row's.
		if id, ok := authn.ShortIDOf(pw); !ok || id != c.ShortID || !strings.HasPrefix(pw, "txc_"+c.ShortID+"_") {
			t.Errorf("token password %q does not carry short id %q", pw, c.ShortID)
		}
		got, ok, err := s.VerifyPassword(ctx, tn, pony, pw)
		if err != nil || !ok || got.ID != c.ID || !got.Scopes.Allows("imap", "paris@onepony.com", "login") || got.Scopes.Allows("ipp", "front-desk", "print") {
			t.Errorf("verify: %+v ok=%v err=%v", got, ok, err)
		}

		// The form a person types.
		wc, wpw, err := s.IssueCredential(ctx, tn, "web/canary", pony, authn.NewCredential{Scopes: []string{"calendar:*:*"}, Style: authn.StyleWords})
		if err != nil {
			t.Fatal(err)
		}
		if parts := strings.Split(wpw, "-"); len(parts) != 6 || parts[0] != wc.ShortID {
			t.Errorf("words password = %q (short id %q)", wpw, wc.ShortID)
		}
		if _, ok, _ := s.VerifyPassword(ctx, tn, pony, wpw); !ok {
			t.Error("words password does not verify")
		}

		for name, bad := range map[string]string{
			"wrong secret":   pw[:len(pw)-1] + "x",
			"no id (legacy)": "river-galaxy-bamboo-orbit-velvet",
			"unknown id":     "txc_zzzz_" + strings.Repeat("a", 32),
			"empty":          "",
		} {
			if _, ok, err := s.VerifyPassword(ctx, tn, pony, bad); ok || err != nil {
				t.Errorf("%s: ok=%v err=%v", name, ok, err)
			}
		}
		// A password opens its own principal only, in its own tenant only.
		other, _ := authn.ParsePrincipal("pony:bergen")
		if _, ok, _ := s.VerifyPassword(ctx, tn, other, pw); ok {
			t.Error("password verified for another principal")
		}
		if _, ok, _ := s.VerifyPassword(ctx, tenant(), pony, pw); ok {
			t.Error("password verified in another tenant")
		}

		list, err := s.ListCredentials(ctx, tn, "web", pony, false)
		if err != nil || len(list) != 2 || list[0].ID != wc.ID || list[1].ID != c.ID {
			t.Fatalf("list (newest first): %+v err=%v", list, err)
		}

		rc, revoked, err := s.RevokeCredential(ctx, tn, "web", c.ID)
		if err != nil || !revoked || !rc.Revoked() {
			t.Fatalf("revoke: %+v revoked=%v err=%v", rc, revoked, err)
		}
		if _, again, err := s.RevokeCredential(ctx, tn, "web", c.ID); err != nil || again {
			t.Errorf("second revoke: revoked=%v err=%v", again, err)
		}
		if _, ok, _ := s.VerifyPassword(ctx, tn, pony, pw); ok {
			t.Error("revoked password still verifies")
		}
		if list, _ := s.ListCredentials(ctx, tn, "web", pony, false); len(list) != 1 {
			t.Errorf("live list after revoke: %d", len(list))
		}
		if list, _ := s.ListCredentials(ctx, tn, "web", pony, true); len(list) != 2 {
			t.Errorf("full list after revoke: %d", len(list))
		}
		if _, _, err := s.RevokeCredential(ctx, tn, "core", wc.ID); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("revoke from another stack: %v", err)
		}
		if _, _, err := s.RevokeCredential(ctx, tn, "web", "crd_missing"); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("revoke a missing credential: %v", err)
		}
		if _, _, err := s.RevokeCredential(ctx, tenant(), "web", wc.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant revoke: %v", err)
		}
	})

	t.Run("rotation revokes every credential but the new one", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		pony, _ := authn.ParsePrincipal("pony:lima")
		var pws []string
		var last authn.Credential
		for i := 0; i < 3; i++ {
			c, pw, err := s.IssueCredential(ctx, tn, "web", pony, authn.NewCredential{Scopes: []string{"imap:*:*"}})
			if err != nil {
				t.Fatal(err)
			}
			pws, last = append(pws, pw), c
		}
		if _, err := s.RevokeCredentials(ctx, tn, "core", pony, last.ID); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("rotate from another stack: %v", err)
		}
		// `except` must name a live credential of THIS principal: a missing id
		// (the issue step failed) must not revoke the passwords that work.
		elsewhere, _ := authn.ParsePrincipal("pony:quito")
		foreign, _, _ := s.IssueCredential(ctx, tn, "web", elsewhere, authn.NewCredential{Scopes: []string{"imap:*:*"}})
		for name, except := range map[string]string{"unknown id": "crd_missing", "another principal's": foreign.ID} {
			if n, err := s.RevokeCredentials(ctx, tn, "web", pony, except); !errors.Is(err, authn.ErrNotFound) || n != 0 {
				t.Errorf("except = %s: revoked %d err=%v", name, n, err)
			}
		}
		n, err := s.RevokeCredentials(ctx, tn, "web", pony, last.ID)
		if err != nil || n != 2 {
			t.Fatalf("revoked %d err=%v", n, err)
		}
		for i, pw := range pws {
			if _, ok, _ := s.VerifyPassword(ctx, tn, pony, pw); ok != (i == 2) {
				t.Errorf("password %d verifies=%v", i, ok)
			}
		}
		// Ownership is sticky: with every credential revoked, the pony still
		// belongs to `web`.
		if n, err := s.RevokeCredentials(ctx, tn, "web", pony, ""); err != nil || n != 1 {
			t.Errorf("revoke all: %d err=%v", n, err)
		}
		if _, _, err := s.IssueCredential(ctx, tn, "core", pony, authn.NewCredential{Scopes: []string{"imap:*:*"}}); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("claim a fully revoked principal: %v", err)
		}
	})

	t.Run("credential arguments and the live cap", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		pony, _ := authn.ParsePrincipal("pony:cairo")
		for name, in := range map[string]authn.NewCredential{
			"no scopes":       {},
			"wildcard domain": {Scopes: []string{"*:*:*"}},
			"two segments":    {Scopes: []string{"imap:login"}},
			"alias":           {Scopes: []string{"admin:all"}},
			"bad style":       {Scopes: []string{"imap:*:*"}, Style: "pin"},
			"too few words":   {Scopes: []string{"imap:*:*"}, Style: authn.StyleWords, Words: 3},
			"long label":      {Scopes: []string{"imap:*:*"}, Label: strings.Repeat("x", 129)},
		} {
			if _, _, err := s.IssueCredential(ctx, tn, "web", pony, in); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A refused issue writes nothing, so it claims nothing.
		if _, _, err := s.IssueCredential(ctx, tn, "core", pony, authn.NewCredential{Scopes: []string{"imap:*:*"}}); err != nil {
			t.Fatalf("the pony was claimed by a refused issue: %v", err)
		}

		full, _ := authn.ParsePrincipal("pony:full")
		insert := s.Dialect.Rebind(`INSERT INTO credentials
			(id, tenant_id, principal_id, short_id, kind, secret_hash, scopes, label, created_by, created_at)
			VALUES (?, ?, 'pony:full', ?, 'app_password', 'x', '["imap:*:*"]', '', 'web', '2026-09-18T00:00:00Z')`)
		for i := 0; i < authn.MaxCredentials; i++ {
			id := "fill" + hxid.NewTimeSort().String()
			if _, err := s.DB.ExecContext(ctx, insert, "crd_"+id, tn, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := s.IssueCredential(ctx, tn, "web", full, authn.NewCredential{Scopes: []string{"imap:*:*"}}); !errors.Is(err, authn.ErrTooMany) {
			t.Errorf("issue past the cap: %v", err)
		}
	})

	t.Run("resolver", func(t *testing.T) { resolverCases(t, newStore) })
}

// SameTenantID is a ResolverConfig.TenantID for tests whose tenants use the
// same string for slug and id.
func SameTenantID(_ context.Context, slug string) (string, error) { return slug, nil }

func sameTenantID(ctx context.Context, slug string) (string, error) { return SameTenantID(ctx, slug) }

// Grant makes username sign in as p with password, for a head's tests —
// which need to know the password, where the store only ever generates
// one. It binds the username (as stack `web`), then gives p a credential
// holding exactly password with the given scopes. password must carry a
// short id, as an issued one does (authn.ShortIDOf): "bcdf-secret" does.
// Granting the same short id to p again replaces its password.
func Grant(t testing.TB, s *authn.Store, tenantID string, p authn.Principal, username, password string, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := s.BindPrincipal(ctx, tenantID, "web", p, authn.NewBinding{Kind: authn.BindEmail, Subject: username}); err != nil {
		t.Fatalf("bind %s: %v", username, err)
	}
	short, ok := authn.ShortIDOf(password)
	if !ok {
		t.Fatalf("test password %q has no credential id: write it like bcdf-%s", password, password)
	}
	hash, err := apppass.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authn.ParseScopes(scopes); err != nil {
		t.Fatalf("scopes: %v", err)
	}
	enc, _ := json.Marshal(scopes)
	if _, err := s.DB.ExecContext(ctx, s.Dialect.Rebind(
		`DELETE FROM credentials WHERE tenant_id = ? AND principal_id = ? AND short_id = ?`), tenantID, p.ID, short); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, s.Dialect.Rebind(`INSERT INTO credentials
		(id, tenant_id, principal_id, short_id, kind, secret_hash, scopes, label, created_by, created_at)
		VALUES (?, ?, ?, ?, 'app_password', ?, ?, 'test', 'web', '2026-09-19T00:00:00Z')`),
		"crd_"+hxid.NewTimeSort().String(), tenantID, p.ID, short, hash, string(enc)); err != nil {
		t.Fatalf("grant %s: %v", username, err)
	}
}

func resolverCases(t *testing.T, newStore func(t *testing.T) *authn.Store) {
	ctx := context.Background()
	imapLogin := func(username string) authn.Scope {
		return authn.Scope{Domain: "imap", Instance: username, Action: "login"}
	}
	attempt := func(tn, username, pw string) authn.Attempt {
		return authn.Attempt{Tenant: tn, Username: username, Password: pw, IP: "192.0.2.1", Want: imapLogin(username)}
	}

	t.Run("a bound username and its credential sign in", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		r := authn.NewResolver(s, authn.ResolverConfig{TenantID: sameTenantID})
		pony, _ := authn.ParsePrincipal("pony:paris")
		if _, _, err := s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "paris@onepony.com"}); err != nil {
			t.Fatal(err)
		}
		c, pw, err := s.IssueCredential(ctx, tn, "web", pony, authn.NewCredential{Scopes: []string{"imap:*:login", "drive:dc_1:*"}})
		if err != nil {
			t.Fatal(err)
		}
		res := r.Login(ctx, attempt(tn, "Paris@OnePony.com", pw))
		if res.Outcome != authn.OutcomeOK || res.Who.Principal != pony || res.Who.Credential != c.ID || res.Cached {
			t.Fatalf("login = %+v", res)
		}
		// The next one is a cache hit, and records nothing new.
		if res := r.Login(ctx, attempt(tn, "paris@onepony.com", pw)); res.Outcome != authn.OutcomeOK || !res.Cached {
			t.Errorf("second login = %+v", res)
		}
		list, _ := s.ListCredentials(ctx, tn, "web", pony, false)
		if len(list) != 1 || list[0].LastUsedAt == nil {
			t.Errorf("last_used_at not recorded: %+v", list)
		}

		// Doors: the drive scope names one collection; calendar is not named.
		for _, c := range []struct {
			want authn.Scope
			out  authn.Outcome
		}{
			{authn.Scope{Domain: "drive", Instance: "dc_1", Action: "login"}, authn.OutcomeOK},
			{authn.Scope{Domain: "drive", Instance: "dc_2", Action: "login"}, authn.OutcomeScope},
			{authn.Scope{Domain: "calendar", Instance: "paris@onepony.com", Action: "login"}, authn.OutcomeScope},
		} {
			a := attempt(tn, "paris@onepony.com", pw)
			a.Want = c.want
			if res := r.Login(ctx, a); res.Outcome != c.out {
				t.Errorf("door %v: %s, want %s", c.want, res.Outcome, c.out)
			}
		}

		for name, bad := range map[string]string{
			"wrong secret":    pw[:len(pw)-1] + "x",
			"legacy password": "river-galaxy-bamboo-orbit-velvet",
			"unknown id":      "txc_zzzz_" + strings.Repeat("a", 32),
		} {
			if res := r.Login(ctx, attempt(tn, "paris@onepony.com", bad)); res.Outcome != authn.OutcomeFailed {
				t.Errorf("%s: %+v", name, res)
			}
		}
		// Another tenant's same address knows nothing of this pony.
		if res := r.Login(ctx, attempt(tenant(), "paris@onepony.com", pw)); res.Outcome != authn.OutcomeFailed {
			t.Errorf("another tenant: %+v", res)
		}
		// An unbound username in the tenant fails the same way.
		if res := r.Login(ctx, attempt(tn, "milan@onepony.com", pw)); res.Outcome != authn.OutcomeFailed {
			t.Errorf("unbound username: %+v", res)
		}
	})

	t.Run("revocation is immediate, cache or not", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		r := authn.NewResolver(s, authn.ResolverConfig{TenantID: sameTenantID})
		pony, _ := authn.ParsePrincipal("pony:oslo")
		_, _, _ = s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "oslo@onepony.com"})
		c, pw, _ := s.IssueCredential(ctx, tn, "web", pony, authn.NewCredential{Scopes: []string{"imap:*:*"}})
		if res := r.Login(ctx, attempt(tn, "oslo@onepony.com", pw)); res.Outcome != authn.OutcomeOK {
			t.Fatalf("login = %+v", res)
		}
		if _, _, err := s.RevokeCredential(ctx, tn, "web", c.ID); err != nil {
			t.Fatal(err)
		}
		if res := r.Login(ctx, attempt(tn, "oslo@onepony.com", pw)); res.Outcome != authn.OutcomeFailed {
			t.Errorf("revoked, still cached on this node: %+v", res)
		}
	})

	t.Run("a disabled user cannot sign in, and can again once enabled", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		r := authn.NewResolver(s, authn.ResolverConfig{TenantID: sameTenantID})
		u, _, _, err := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "alice@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		_, pw, _ := s.IssueCredential(ctx, tn, "web", u.Principal(), authn.NewCredential{Scopes: []string{"imap:*:*"}})
		if res := r.Login(ctx, attempt(tn, "alice@example.com", pw)); res.Outcome != authn.OutcomeOK || res.Who.Principal != u.Principal() {
			t.Fatalf("login = %+v", res)
		}
		_, _ = s.SetUserDisabled(ctx, tn, "web", u.ID, true)
		if res := r.Login(ctx, attempt(tn, "alice@example.com", pw)); res.Outcome != authn.OutcomeDisabled {
			t.Errorf("disabled = %+v", res)
		}
		_, _ = s.SetUserDisabled(ctx, tn, "web", u.ID, false)
		if res := r.Login(ctx, attempt(tn, "alice@example.com", pw)); res.Outcome != authn.OutcomeOK {
			t.Errorf("enabled again = %+v", res)
		}
	})

	t.Run("checks are throttled per principal across heads, hits are free", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		r := authn.NewResolver(s, authn.ResolverConfig{Rate: 3, TenantID: sameTenantID})
		pony, _ := authn.ParsePrincipal("pony:lima")
		_, _, _ = s.BindPrincipal(ctx, tn, "web", pony, authn.NewBinding{Kind: authn.BindEmail, Subject: "lima@onepony.com"})
		_, pw, _ := s.IssueCredential(ctx, tn, "web", pony, authn.NewCredential{Scopes: []string{"imap:*:*", "calendar:*:*"}})
		// The right password, many times: one check, the rest cache hits.
		for i := 0; i < 10; i++ {
			if res := r.Login(ctx, attempt(tn, "lima@onepony.com", pw)); res.Outcome != authn.OutcomeOK {
				t.Fatalf("login %d = %+v", i, res)
			}
		}
		// Two wrong guesses from different addresses, over two heads, spend
		// the principal's budget; the third check is refused.
		a := attempt(tn, "lima@onepony.com", pw+"x")
		a.IP = "198.51.100.1"
		r.Login(ctx, a)
		a.IP, a.Want = "198.51.100.2", authn.Scope{Domain: "calendar", Instance: "lima@onepony.com", Action: "login"}
		r.Login(ctx, a)
		a.IP = "198.51.100.3"
		if res := r.Login(ctx, a); res.Outcome != authn.OutcomeThrottled {
			t.Errorf("third check = %+v", res)
		}
		// …while the owner's cached password still works.
		if res := r.Login(ctx, attempt(tn, "lima@onepony.com", pw)); res.Outcome != authn.OutcomeOK {
			t.Errorf("cached owner = %+v", res)
		}
	})

	t.Run("no tenant id is an error, not a wrong password", func(t *testing.T) {
		s := newStore(t)
		r := authn.NewResolver(s, authn.ResolverConfig{TenantID: func(context.Context, string) (string, error) {
			return "", errors.New("mirror not loaded")
		}})
		if res := r.Login(ctx, attempt("acme", "a@example.com", "x")); res.Outcome != authn.OutcomeError || res.Err == nil {
			t.Errorf("login = %+v", res)
		}
	})
}
