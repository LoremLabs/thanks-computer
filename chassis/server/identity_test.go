package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newIdentityDeps builds the op deps over a migrated SQLite auth DB and a
// mirror that knows the given tenant slugs (slug → "tnt_"+slug).
func newIdentityDeps(t *testing.T, slugs ...string) identityDeps {
	t.Helper()
	mirror, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	if _, err := mirror.Exec(`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, slug := range slugs {
		if _, err := mirror.Exec(`INSERT INTO tenants VALUES (?, ?, NULL)`, "tnt_"+slug, slug); err != nil {
			t.Fatal(err)
		}
	}
	return identityDeps{store: authntest.NewSQLiteStore(t), snap: func() *sql.DB { return mirror }}
}

type identityOp func(context.Context, identityDeps, []byte) (event.Payload, error)

// callIdentity runs one op as a rule of `stack` in `tenant`.
func callIdentity(t *testing.T, fn identityOp, d identityDeps, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	// The envelope claims another stack and tenant: neither may be believed.
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"evil/0/x","tenant":"evil","stack":"evil"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func wantIdentityCode(t *testing.T, what, out, path, code string) {
	t.Helper()
	if got := gjson.Get(out, path+".error.code").String(); got != code {
		t.Errorf("%s: error code %q, want %q: %s", what, got, code, out)
	}
}

func TestUserOps(t *testing.T) {
	d := newIdentityDeps(t, "acme", "other")

	out := callIdentity(t, userCreate, d, "acme", "web", `{"email":"Alice@Example.com","display_name":"Alice"}`)
	id := gjson.Get(out, "_user.id").String()
	if !strings.HasPrefix(id, "usr_") || gjson.Get(out, "_user.principal").String() != "user:"+id ||
		gjson.Get(out, "_user.email").String() != "alice@example.com" || gjson.Get(out, "_user.email_verified").Bool() ||
		gjson.Get(out, "_user.status").String() != "active" || gjson.Get(out, "_user.created_by").String() != "web" ||
		!gjson.Get(out, "_user.created").Bool() || gjson.Get(out, "_user.display_name").String() != "Alice" {
		t.Fatalf("create = %s", out)
	}
	// The row is keyed by the tenant's ID, resolved from the pinned slug.
	if u, err := d.store.GetUser(context.Background(), "tnt_acme", id); err != nil || u.TenantID != "tnt_acme" {
		t.Errorf("stored user: %+v err=%v", u, err)
	}

	// Safe to retry, from the stack or its canary slot; `verified` upgrades.
	out = callIdentity(t, userCreate, d, "acme", "web/canary", `{"email":"alice@example.com","verified":true,"into":"u"}`)
	if gjson.Get(out, "u.id").String() != id || gjson.Get(out, "u.created").Bool() || !gjson.Get(out, "u.email_verified").Bool() {
		t.Errorf("re-create = %s", out)
	}
	// Another stack is told who to ask.
	out = callIdentity(t, userCreate, d, "acme", "ingest", `{"email":"alice@example.com"}`)
	wantIdentityCode(t, "create from another stack", out, "_user", "txco_user_not_owner")
	if msg := gjson.Get(out, "_user.error.message").String(); !strings.Contains(msg, `"web"`) || strings.Contains(msg, "authn:") {
		t.Errorf("not_owner message should name the owner, in the author's terms: %q", msg)
	}
	// The same address in another tenant is another person.
	out = callIdentity(t, userCreate, d, "other", "web", `{"email":"alice@example.com"}`)
	if other := gjson.Get(out, "_user.id").String(); other == "" || other == id || !gjson.Get(out, "_user.created").Bool() {
		t.Errorf("other tenant = %s", out)
	}

	// get: by id or email, from any stack; never across tenants.
	out = callIdentity(t, userGet, d, "acme", "ingest", `{"id":"`+id+`"}`)
	if gjson.Get(out, "_user.id").String() != id || gjson.Get(out, "_user.email").Exists() {
		t.Errorf("get by id = %s", out)
	}
	out = callIdentity(t, userGet, d, "acme", "ingest", `{"email":"ALICE@example.com"}`)
	if gjson.Get(out, "_user.id").String() != id || !gjson.Get(out, "_user.email_verified").Bool() {
		t.Errorf("get by email = %s", out)
	}
	wantIdentityCode(t, "get across tenants", callIdentity(t, userGet, d, "other", "web", `{"id":"`+id+`"}`), "_user", "txco_user_not_found")
	wantIdentityCode(t, "get unknown email", callIdentity(t, userGet, d, "acme", "web", `{"email":"nobody@example.com"}`), "_user", "txco_user_not_found")
	wantIdentityCode(t, "get with both", callIdentity(t, userGet, d, "acme", "web", `{"id":"`+id+`","email":"alice@example.com"}`), "_user", "txco_user_invalid_arg")
	wantIdentityCode(t, "get with neither", callIdentity(t, userGet, d, "acme", "web", `{}`), "_user", "txco_user_invalid_arg")

	// disable / enable: the owner only.
	wantIdentityCode(t, "disable from another stack", callIdentity(t, userDisable, d, "acme", "ingest", `{"id":"`+id+`"}`), "_user", "txco_user_not_owner")
	out = callIdentity(t, userDisable, d, "acme", "web", `{"id":"`+id+`"}`)
	if gjson.Get(out, "_user.status").String() != "disabled" {
		t.Errorf("disable = %s", out)
	}
	wantIdentityCode(t, "issue to a disabled user", callIdentity(t, credentialCreate, d, "acme", "web",
		`{"principal":"user:`+id+`","scopes":["imap:*:login"]}`), "_credential", "txco_credential_user_disabled")
	out = callIdentity(t, userDisable, d, "acme", "web", `{"id":"`+id+`","disabled":false}`)
	if gjson.Get(out, "_user.status").String() != "active" {
		t.Errorf("enable = %s", out)
	}
	wantIdentityCode(t, "disable without id", callIdentity(t, userDisable, d, "acme", "web", `{}`), "_user", "txco_user_invalid_arg")
	out = callIdentity(t, userCreate, d, "acme", "web", `{"email":"nope"}`)
	wantIdentityCode(t, "bad email", out, "_user", "txco_user_invalid_arg")
	if msg := gjson.Get(out, "_user.error.message").String(); !strings.HasPrefix(msg, "email ") {
		t.Errorf("invalid_arg message should start with what to fix: %q", msg)
	}
}

func TestCredentialOps(t *testing.T) {
	d := newIdentityDeps(t, "acme")

	out := callIdentity(t, credentialCreate, d, "acme", "web",
		`{"principal":"pony:paris","scopes":["imap:*:*","calendar:*:*"],"label":"Laptop","password_style":"words"}`)
	id, pw := gjson.Get(out, "_credential.id").String(), gjson.Get(out, "_credential.password").String()
	short := gjson.Get(out, "_credential.short_id").String()
	if !strings.HasPrefix(id, "crd_") || gjson.Get(out, "_credential.principal").String() != "pony:paris" ||
		gjson.Get(out, "_credential.kind").String() != "app_password" || gjson.Get(out, "_credential.label").String() != "Laptop" ||
		gjson.Get(out, "_credential.scopes.#").Int() != 2 || short == "" || !strings.HasPrefix(pw, short+"-") || strings.Count(pw, "-") != 5 {
		t.Fatalf("create = %s", out)
	}
	pony, _ := authn.ParsePrincipal("pony:paris")
	if c, ok, err := d.store.VerifyPassword(context.Background(), "tnt_acme", pony, pw); err != nil || !ok || c.ID != id {
		t.Errorf("issued password does not verify: ok=%v err=%v", ok, err)
	}
	// `scopes` may be one string; the default style is the machine token.
	out = callIdentity(t, credentialCreate, d, "acme", "web", `{"principal":"pony:paris","scopes":"ipp:front-desk:print"}`)
	second, tok := gjson.Get(out, "_credential.id").String(), gjson.Get(out, "_credential.password").String()
	if second == "" || !strings.HasPrefix(tok, "txc_"+gjson.Get(out, "_credential.short_id").String()+"_") {
		t.Fatalf("token create = %s", out)
	}

	for what, meta := range map[string]string{
		"a chosen password":   `{"principal":"pony:paris","scopes":["imap:*:*"],"password":"hunter2hunter2"}`,
		"no principal":        `{"scopes":["imap:*:*"]}`,
		"a malformed one":     `{"principal":"paris","scopes":["imap:*:*"]}`,
		"a named user":        `{"principal":"user:paris","scopes":["imap:*:*"]}`,
		"no scopes":           `{"principal":"pony:paris"}`,
		"a wildcard domain":   `{"principal":"pony:paris","scopes":["*:*:*"]}`,
		"an unknown style":    `{"principal":"pony:paris","scopes":["imap:*:*"],"password_style":"pin"}`,
		"too few words":       `{"principal":"pony:paris","scopes":["imap:*:*"],"password_style":"words","password_words":2}`,
		"an overlong label":   `{"principal":"pony:paris","scopes":["imap:*:*"],"label":"` + strings.Repeat("x", 200) + `"}`,
		"a non-string scopes": `{"principal":"pony:paris","scopes":[{"a":1}]}`,
	} {
		wantIdentityCode(t, what, callIdentity(t, credentialCreate, d, "acme", "web", meta), "_credential", "txco_credential_invalid_arg")
	}
	wantIdentityCode(t, "a user that does not exist", callIdentity(t, credentialCreate, d, "acme", "web",
		`{"principal":"user:usr_22222222","scopes":["imap:*:*"]}`), "_credential", "txco_credential_not_found")
	wantIdentityCode(t, "another stack", callIdentity(t, credentialCreate, d, "acme", "core",
		`{"principal":"pony:paris","scopes":["imap:*:*"]}`), "_credential", "txco_credential_not_owner")

	// list: newest first, the owner only, and never a secret.
	out = callIdentity(t, credentialList, d, "acme", "web/canary", `{"principal":"pony:paris"}`)
	if gjson.Get(out, "_credential.count").Int() != 2 || gjson.Get(out, "_credential.items.0.id").String() != second ||
		gjson.Get(out, "_credential.items.1.id").String() != id || gjson.Get(out, "_credential.principal").String() != "pony:paris" {
		t.Errorf("list = %s", out)
	}
	for _, leak := range []string{pw, tok, "argon2", "secret_hash", `"password"`} {
		if strings.Contains(out, leak) {
			t.Errorf("list leaks %q: %s", leak, out)
		}
	}
	wantIdentityCode(t, "list from another stack", callIdentity(t, credentialList, d, "acme", "core", `{"principal":"pony:paris"}`), "_credential", "txco_credential_not_owner")

	// revoke one.
	out = callIdentity(t, credentialRevoke, d, "acme", "web", `{"id":"`+second+`"}`)
	if !gjson.Get(out, "_credential.revoked").Bool() || gjson.Get(out, "_credential.principal").String() != "pony:paris" {
		t.Errorf("revoke = %s", out)
	}
	out = callIdentity(t, credentialRevoke, d, "acme", "web", `{"id":"`+second+`"}`)
	if gjson.Get(out, "_credential.revoked").Bool() || gjson.Get(out, "_credential.error").Exists() {
		t.Errorf("second revoke = %s", out)
	}
	out = callIdentity(t, credentialList, d, "acme", "web", `{"principal":"pony:paris","include_revoked":true}`)
	if gjson.Get(out, "_credential.count").Int() != 2 || gjson.Get(out, "_credential.items.0.revoked_at").String() == "" {
		t.Errorf("list with revoked = %s", out)
	}
	wantIdentityCode(t, "revoke from another stack", callIdentity(t, credentialRevoke, d, "acme", "core", `{"id":"`+id+`"}`), "_credential", "txco_credential_not_owner")
	wantIdentityCode(t, "revoke an unknown id", callIdentity(t, credentialRevoke, d, "acme", "web", `{"id":"crd_nope"}`), "_credential", "txco_credential_not_found")
	wantIdentityCode(t, "revoke with nothing", callIdentity(t, credentialRevoke, d, "acme", "web", `{}`), "_credential", "txco_credential_invalid_arg")
	wantIdentityCode(t, "revoke with both", callIdentity(t, credentialRevoke, d, "acme", "web", `{"id":"`+id+`","principal":"pony:paris"}`), "_credential", "txco_credential_invalid_arg")
}

// TestRotationCannotLockThePrincipalOut — a rotation is create, then revoke
// with `except` = the new id. When the create step fails its id is missing
// from the envelope, so `except` arrives empty; the revoke must then refuse,
// not revoke the one password that still works.
func TestRotationCannotLockThePrincipalOut(t *testing.T) {
	d := newIdentityDeps(t, "acme")
	pony, _ := authn.ParsePrincipal("pony:paris")
	verifies := func(pw string) bool {
		_, ok, _ := d.store.VerifyPassword(context.Background(), "tnt_acme", pony, pw)
		return ok
	}
	out := callIdentity(t, credentialCreate, d, "acme", "web", `{"principal":"pony:paris","scopes":["imap:*:*"]}`)
	oldPw := gjson.Get(out, "_credential.password").String()

	for what, meta := range map[string]string{
		"an empty except":   `{"principal":"pony:paris","except":""}`,
		"no except at all":  `{"principal":"pony:paris"}`,
		"except and all":    `{"principal":"pony:paris","except":"crd_x","all":true}`,
		"all spelled false": `{"principal":"pony:paris","all":false}`,
	} {
		wantIdentityCode(t, what, callIdentity(t, credentialRevoke, d, "acme", "web", meta), "_credential", "txco_credential_invalid_arg")
	}
	wantIdentityCode(t, "an except that names nothing", callIdentity(t, credentialRevoke, d, "acme", "web",
		`{"principal":"pony:paris","except":"crd_missing"}`), "_credential", "txco_credential_not_found")
	if !verifies(oldPw) {
		t.Fatal("a refused rotation revoked the working password")
	}

	// The real thing.
	out = callIdentity(t, credentialCreate, d, "acme", "web", `{"principal":"pony:paris","scopes":["imap:*:*"]}`)
	newID, newPw := gjson.Get(out, "_credential.id").String(), gjson.Get(out, "_credential.password").String()
	out = callIdentity(t, credentialRevoke, d, "acme", "web", `{"principal":"pony:paris","except":"`+newID+`"}`)
	if gjson.Get(out, "_credential.revoked_count").Int() != 1 || verifies(oldPw) || !verifies(newPw) {
		t.Errorf("rotate = %s old=%v new=%v", out, verifies(oldPw), verifies(newPw))
	}
	// Revoking everything is possible, but only by saying so.
	out = callIdentity(t, credentialRevoke, d, "acme", "web", `{"principal":"pony:paris","all":true}`)
	if gjson.Get(out, "_credential.revoked_count").Int() != 1 || verifies(newPw) {
		t.Errorf("revoke all = %s", out)
	}
}

func TestIdentityOpsTrustOnlyThePinnedScope(t *testing.T) {
	d := newIdentityDeps(t, "acme")

	wantIdentityCode(t, "no tenant", callIdentity(t, userCreate, d, "", "web", `{"email":"a@example.com"}`), "_user", "txco_user_no_tenant")
	wantIdentityCode(t, "unknown tenant", callIdentity(t, userCreate, d, "ghost", "web", `{"email":"a@example.com"}`), "_user", "txco_user_no_tenant")
	wantIdentityCode(t, "no stack", callIdentity(t, userCreate, d, "acme", "", `{"email":"a@example.com"}`), "_user", "txco_user_no_stack")
	wantIdentityCode(t, "no stack (credential)", callIdentity(t, credentialCreate, d, "acme", "",
		`{"principal":"pony:paris","scopes":["imap:*:*"]}`), "_credential", "txco_credential_no_stack")

	off := identityDeps{snap: d.snap} // auth.db failed to open on this node
	wantIdentityCode(t, "no store", callIdentity(t, userCreate, off, "acme", "web", `{"email":"a@example.com"}`), "_user", "txco_user_disabled")
	wantIdentityCode(t, "no store (credential)", callIdentity(t, credentialList, off, "acme", "web", `{"principal":"pony:paris"}`), "_credential", "txco_credential_disabled")

	// callIdentity's envelope claims tenant "evil" and stack "evil": the row
	// must carry the pinned ones.
	out := callIdentity(t, userCreate, d, "acme", "web/canary", `{"email":"b@example.com"}`)
	if gjson.Get(out, "_user.created_by").String() != "web" {
		t.Errorf("created_by = %s", out)
	}
	if u, err := d.store.GetUser(context.Background(), "tnt_acme", gjson.Get(out, "_user.id").String()); err != nil || u.CreatedBy != "web" {
		t.Errorf("stored: %+v err=%v", u, err)
	}

	// `into` is an author-chosen target: a reserved one falls back to the
	// default instead of letting a password land in (or forge) `_txc`.
	out = callIdentity(t, credentialCreate, d, "acme", "web", `{"principal":"pony:paris","scopes":["imap:*:*"],"into":"@principal"}`)
	if gjson.Get(out, "_txc").Exists() || gjson.Get(out, "_credential.password").String() == "" {
		t.Errorf("reserved into = %s", out)
	}
}
