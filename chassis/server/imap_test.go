package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newIMAPDeps builds the op deps over a temp SQLite index + file CAS, with
// a mirror DB that owns exactly the given (tenant, hostname) pairs.
func newIMAPDeps(t *testing.T, owned map[string]string) imapDeps {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "imap.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := chimap.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A minimal mirror with the two tables DomainOwnedByTenant reads.
	mirror, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE tenant_hostnames (hostname TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
		`CREATE TABLE dns_zones (origin TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
	} {
		if _, err := mirror.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for host, slug := range owned {
		if _, err := mirror.Exec(`INSERT OR IGNORE INTO tenants VALUES (?, ?, NULL)`, "t_"+slug, slug); err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.Exec(`INSERT INTO tenant_hostnames VALUES (?, ?, '2026-09-03T00:00:00Z', NULL)`, host, "t_"+slug); err != nil {
			t.Fatal(err)
		}
	}
	fixed := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	return imapDeps{
		store: store, fcas: fs, ix: blob.NewKVIndex(newKVHandle(t)),
		snap: func() *sql.DB { return mirror }, dialect: registry.SQLite,
		maxBytes: 1 << 20, now: func() time.Time { return fixed },
	}
}

type imapHandler func(context.Context, imapDeps, []byte) (event.Payload, error)

func callIMAP(t *testing.T, fn imapHandler, d imapDeps, tenant, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"demo/100/imap"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestIMAPAccountOp(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})

	out := callIMAP(t, imapAccount, d, "acme", `{"username":"Paris@Pony.Example.com"}`)
	if gjson.Get(out, "_imap.error").Exists() {
		t.Fatalf("create: %s", out)
	}
	pw := gjson.Get(out, "_imap.password").String()
	if gjson.Get(out, "_imap.username").String() != "paris@pony.example.com" || !gjson.Get(out, "_imap.created").Bool() || len(pw) != 29 {
		t.Errorf("create = %s", out)
	}
	a, ok, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if !ok || a.Tenant != "acme" {
		t.Fatalf("account = %+v ok=%v", a, ok)
	}
	if match, _ := chimap.VerifyPassword(a.PwHash, pw); !match {
		t.Error("generated password does not verify")
	}
	if _, ok, _ := d.store.GetMailbox(context.Background(), "acme", "paris@pony.example.com", "INBOX"); !ok {
		t.Error("INBOX not created")
	}

	// Update without a password: no rotation, no password in the output.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","status":"disabled","into":"_acct"}`)
	if gjson.Get(out, "_acct.created").Bool() || gjson.Get(out, "_acct.password").Exists() || gjson.Get(out, "_acct.error").Exists() {
		t.Errorf("update = %s", out)
	}
	a2, _, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if a2.PwHash != a.PwHash || a2.Status != chimap.StatusDisabled {
		t.Errorf("update changed hash or missed status: %+v", a2)
	}
	// Explicit password is stored; too short is refused.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","password":"correct horse battery"}`)
	if gjson.Get(out, "_imap.error").Exists() || gjson.Get(out, "_imap.password").Exists() {
		t.Errorf("set password = %s", out)
	}
	a3, _, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if match, _ := chimap.VerifyPassword(a3.PwHash, "correct horse battery"); !match {
		t.Error("explicit password not stored")
	}
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"x@pony.example.com","password":"short"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_invalid_arg" {
		t.Errorf("short password = %s", out)
	}

	// Errors: domain not owned, other tenant's username, bad username,
	// no tenant, no store.
	for _, tc := range []struct{ tenant, meta, code string }{
		{"acme", `{"username":"paris@other.example.com"}`, "txco_imap_domain_not_owned"},
		{"acme", `{"username":"not-an-address"}`, "txco_imap_invalid_arg"},
		{"acme", `{"username":".dot@pony.example.com"}`, "txco_imap_invalid_arg"},
		{"", `{"username":"paris@pony.example.com"}`, "txco_imap_no_tenant"},
	} {
		out := callIMAP(t, imapAccount, d, tc.tenant, tc.meta)
		if got := gjson.Get(out, "_imap.error.code").String(); got != tc.code {
			t.Errorf("%s %s → %q, want %s (%s)", tc.tenant, tc.meta, got, tc.code, out)
		}
	}
	d2 := newIMAPDeps(t, map[string]string{"pony.example.com": "rival"})
	d2.store = d.store // same index, different tenant owns the domain
	out = callIMAP(t, imapAccount, d2, "rival", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_username_taken" {
		t.Errorf("cross-tenant = %s", out)
	}
	out = callIMAP(t, imapAccount, imapDeps{}, "acme", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_disabled" {
		t.Errorf("no store = %s", out)
	}
}

func TestIMAPAppendOp(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})
	callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com"}`)

	meta := `{"username":"paris@pony.example.com","object_key":"hello","flags":["$Hello"],
	  "message":{"from":"Paris <paris@pony.example.com>","to":"owner@example.com","subject":"Hello","text":"Welcome.\n",
	             "attachments":[{"name":"guide.pdf","content_type":"application/pdf","size":10}]}}`
	out := callIMAP(t, imapAppend, d, "acme", meta)
	if gjson.Get(out, "_imap.error").Exists() {
		t.Fatalf("append: %s", out)
	}
	if gjson.Get(out, "_imap.uid").Int() != 1 || gjson.Get(out, "_imap.noop").Bool() || gjson.Get(out, "_imap.mailbox").String() != "INBOX" {
		t.Errorf("append = %s", out)
	}
	sha := gjson.Get(out, "_imap.sha256").String()
	if ok, _ := d.fcas.Exists(context.Background(), sha); !ok {
		t.Error("record not in CAS")
	}
	if _, ok, _ := d.ix.GetSha(context.Background(), "acme", sha); !ok {
		t.Error("tenant sha row not recorded")
	}
	mb, _, _ := d.store.GetMailbox(context.Background(), "acme", "paris@pony.example.com", "INBOX")
	m, ok, _ := d.store.GetMessage(context.Background(), mb.ID, 1)
	if !ok || m.Kind != chimap.KindRecord || m.Subject != "Hello" || m.FromAddr != "paris@pony.example.com" ||
		!strings.Contains(string(m.Parts), `"guide.pdf"`) || !chimap.HasFlag(m.Flags, "$Hello") || !m.InternalDate.Equal(d.now()) {
		t.Errorf("row = %+v", m)
	}
	// Same key, same content → noop; different content → replaced with uid 2.
	out = callIMAP(t, imapAppend, d, "acme", meta)
	if !gjson.Get(out, "_imap.noop").Bool() || gjson.Get(out, "_imap.uid").Int() != 1 {
		t.Errorf("noop = %s", out)
	}
	out = callIMAP(t, imapAppend, d, "acme", strings.Replace(meta, "Welcome.", "Welcome back.", 1))
	if !gjson.Get(out, "_imap.replaced").Bool() || gjson.Get(out, "_imap.uid").Int() != 2 {
		t.Errorf("replace = %s", out)
	}
	// Errors.
	for _, tc := range []struct{ tenant, meta, code string }{
		{"acme", `{"username":"paris@pony.example.com","object_key":"k","from":"@lmtp.msg.raw"}`, "txco_imap_invalid_arg"},
		{"acme", `{"username":"paris@pony.example.com","message":{"text":"x"}}`, "txco_imap_invalid_arg"},
		{"acme", `{"username":"paris@pony.example.com","object_key":"k","message":{"subject":"only"}}`, "txco_imap_invalid_arg"},
		{"acme", `{"username":"paris@pony.example.com","object_key":"k","mailbox":"Nope","message":{"text":"x"}}`, "txco_imap_no_mailbox"},
		{"acme", `{"username":"paris@pony.example.com","object_key":"k","mailbox":"role:knowledge","message":{"text":"x"}}`, "txco_imap_no_mailbox"},
		{"acme", `{"username":"ghost@pony.example.com","object_key":"k","message":{"text":"x"}}`, "txco_imap_no_account"},
		{"rival", `{"username":"paris@pony.example.com","object_key":"k","message":{"text":"x"}}`, "txco_imap_no_account"},
		{"acme", `{"username":"paris@pony.example.com","object_key":"k","internaldate":"yesterday","message":{"text":"x"}}`, "txco_imap_invalid_arg"},
	} {
		out := callIMAP(t, imapAppend, d, tc.tenant, tc.meta)
		if got := gjson.Get(out, "_imap.error.code").String(); got != tc.code {
			t.Errorf("%s → %q, want %s (%s)", tc.meta, got, tc.code, out)
		}
	}
	small := d
	small.maxBytes = 100
	out = callIMAP(t, imapAppend, small, "acme", strings.Replace(meta, `"object_key":"hello"`, `"object_key":"big"`, 1))
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_too_large" {
		t.Errorf("too large = %s", out)
	}
}

// TestIMAPAccountOpWordsAndRotate covers the studio-facing password
// shapes: `password_style = "words"` (BIP-39 words, default 5) and
// `rotate = true` (regenerate for an existing account, returned once).
func TestIMAPAccountOpWordsAndRotate(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})
	wordShape := func(t *testing.T, pw string, n int) {
		t.Helper()
		parts := strings.Split(pw, "-")
		if len(parts) != n {
			t.Fatalf("password %q has %d words, want %d", pw, len(parts), n)
		}
		for _, w := range parts {
			if w == "" || strings.ToLower(w) != w || strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" {
				t.Fatalf("password %q: word %q is not a lowercase word", pw, w)
			}
		}
	}
	verify := func(t *testing.T, pw string) bool {
		t.Helper()
		a, ok, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
		if !ok {
			t.Fatal("account missing")
		}
		m, _ := chimap.VerifyPassword(a.PwHash, pw)
		return m
	}

	// Create with words (default 5).
	out := callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","password_style":"words"}`)
	if gjson.Get(out, "_imap.error").Exists() || !gjson.Get(out, "_imap.created").Bool() || gjson.Get(out, "_imap.rotated").Exists() {
		t.Fatalf("create words = %s", out)
	}
	pw1 := gjson.Get(out, "_imap.password").String()
	wordShape(t, pw1, 5)
	if !verify(t, pw1) {
		t.Error("word password does not verify")
	}

	// Rotate: a new generated password, returned once; the old one is dead.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","password_style":"words","password_words":6,"rotate":true}`)
	if gjson.Get(out, "_imap.error").Exists() || gjson.Get(out, "_imap.created").Bool() || !gjson.Get(out, "_imap.rotated").Bool() {
		t.Fatalf("rotate = %s", out)
	}
	pw2 := gjson.Get(out, "_imap.password").String()
	wordShape(t, pw2, 6)
	if pw2 == pw1 || verify(t, pw1) || !verify(t, pw2) {
		t.Errorf("rotation did not replace the password (pw1 verifies=%v pw2 verifies=%v)", verify(t, pw1), verify(t, pw2))
	}

	// Rotate with an explicit password: that password, nothing returned.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","rotate":true,"password":"chosen-by-owner"}`)
	if gjson.Get(out, "_imap.error").Exists() || gjson.Get(out, "_imap.password").Exists() || gjson.Get(out, "_imap.rotated").Exists() {
		t.Errorf("rotate explicit = %s", out)
	}
	if !verify(t, "chosen-by-owner") {
		t.Error("explicit rotation password not stored")
	}

	// Plain update still leaves the password alone.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com","status":"active"}`)
	if gjson.Get(out, "_imap.password").Exists() || !verify(t, "chosen-by-owner") {
		t.Errorf("update rotated unexpectedly: %s", out)
	}

	// Token style is still the default; rotate on a NEW account is just create.
	out = callIMAP(t, imapAccount, d, "acme", `{"username":"rome@pony.example.com","rotate":true}`)
	if !gjson.Get(out, "_imap.created").Bool() || gjson.Get(out, "_imap.rotated").Exists() || len(gjson.Get(out, "_imap.password").String()) != 29 {
		t.Errorf("rotate on new = %s", out)
	}

	// Bad arguments.
	for _, meta := range []string{
		`{"username":"paris@pony.example.com","password_style":"haiku"}`,
		`{"username":"paris@pony.example.com","password_style":"words","password_words":3}`,
		`{"username":"paris@pony.example.com","password_style":"words","password_words":13}`,
	} {
		if out := callIMAP(t, imapAccount, d, "acme", meta); gjson.Get(out, "_imap.error.code").String() != "txco_imap_invalid_arg" {
			t.Errorf("%s → %s, want txco_imap_invalid_arg", meta, out)
		}
	}
}
