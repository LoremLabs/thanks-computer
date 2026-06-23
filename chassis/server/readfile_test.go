package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

// --- helpers ---------------------------------------------------------------

// fakeCAS is a trivial in-memory filecas.Store for the tenant-CAS path.
type fakeCAS struct{ m map[string][]byte }

func (f *fakeCAS) Put(_ context.Context, hash string, data []byte) error {
	f.m[hash] = data
	return nil
}
func (f *fakeCAS) Get(_ context.Context, hash string) ([]byte, error) {
	b, ok := f.m[hash]
	if !ok {
		return nil, filecas.ErrNotFound
	}
	return b, nil
}
func (f *fakeCAS) Exists(_ context.Context, hash string) (bool, error) {
	_, ok := f.m[hash]
	return ok, nil
}
func (f *fakeCAS) Name() string { return "fake" }

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// inlineIndex builds an Index over a temp workspace holding a routed-stack
// "hello" with a `_`-private template, a robots.txt, and a binary asset.
func inlineIndex(t *testing.T) *static.Index {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string, body []byte) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("OPS/hello/FILES/robots.txt", []byte("STACK"))
	mk("OPS/hello/FILES/_mail/welcome.html", []byte("HELLO-MAIL"))
	mk("OPS/hello/FILES/logo.bin", []byte{0xff, 0xfe, 0x00, 0x01}) // invalid UTF-8
	return static.NewIndex(root, zap.NewNop())
}

func runReadFile(t *testing.T, ix *static.Index, fcas filecas.Store, tenant, stack, metaJSON string, maxBytes int) (event.Payload, error) {
	t.Helper()
	in := `{}`
	in, _ = sjson.Set(in, "_txc.route.stack", stack)
	if tenant != "" {
		in, _ = sjson.Set(in, "_txc.route.tenant", tenant)
	}
	ctx := operation.WithMeta(context.Background(), metaJSON)
	return readFile(ctx, ix, fcas, []byte(in), maxBytes)
}

// --- inline path -----------------------------------------------------------

func TestReadFileInlineDefaults(t *testing.T) {
	ix := inlineIndex(t)
	pay, err := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"_mail/welcome.html","as":"welcome"}]}`, 1<<20)
	if err != nil {
		t.Fatalf("err: %v meta=%s", err, pay.Meta)
	}
	g := gjson.Parse(pay.Raw)
	// Default lands under the private "_files" subtree, as an OBJECT keyed by
	// the alias (the merge-idempotency / no-array-doubling constraint).
	if !g.Get("_files").IsObject() {
		t.Fatalf("_files must be an object; raw=%s", pay.Raw)
	}
	if !g.Get("_files.welcome.found").Bool() {
		t.Fatalf("welcome not found; raw=%s", pay.Raw)
	}
	if got := g.Get("_files.welcome.content").String(); got != "HELLO-MAIL" {
		t.Fatalf("content=%q", got)
	}
	if got := g.Get("_files.welcome.encoding").String(); got != "utf8" {
		t.Fatalf("encoding=%q", got)
	}
	if got := g.Get("_files.welcome.path").String(); got != "_mail/welcome.html" {
		t.Fatalf("path=%q", got)
	}
	if got := g.Get("_files.welcome.size").Int(); got != int64(len("HELLO-MAIL")) {
		t.Fatalf("size=%d", got)
	}
}

func TestReadFileIntoOverrideAndStackPrecedence(t *testing.T) {
	ix := inlineIndex(t)
	pay, err := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"robots.txt","as":"r"}],"into":"data.files"}`, 1<<20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	g := gjson.Parse(pay.Raw)
	if g.Get("_files").Exists() {
		t.Fatalf("default _files must be absent when into is set; raw=%s", pay.Raw)
	}
	if got := g.Get("data.files.r.content").String(); got != "STACK" {
		t.Fatalf("routed-stack robots content=%q", got)
	}
}

// TestReadFileStackParam covers the `stack` WITH override against the DB-backed
// tenant layer (the real path for per-stack FILES; the disk-walk layer is
// single-segment only). A router reads ANOTHER of its tenant's stacks (the nested
// publications/<slug>/_data/index.json) so existence is authoritative — no
// registry. Default reads the routed stack; a nonexistent stack misses; and the
// TRUSTED pinned tenant (TenantScope) beats a spoofed envelope tenant, so `stack`
// can never cross tenants.
func TestReadFileStackParam(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(casDDL); err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) {
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}
	idx := `{"title":"White Fang"}`
	mustExec(`INSERT INTO tenants(tenant_id,slug,name,created_at) VALUES('tnt_a','acme','acme','t')`)
	mustExec(`INSERT INTO stacks(stack_id,tenant_id,name,active_version,created_at) VALUES('s_r','tnt_a','router',1,'t')`)
	mustExec(`INSERT INTO stacks(stack_id,tenant_id,name,active_version,created_at) VALUES('s_p','tnt_a','publications/white-fang',2,'t')`)
	mustExec(`INSERT INTO stack_files(version_id,path,content,content_hash) VALUES(1,'FILES/own.txt','ROUTER',?)`, sha256Hex([]byte("ROUTER")))
	mustExec(`INSERT INTO stack_files(version_id,path,content,content_hash) VALUES(2,'FILES/_data/index.json',?,?)`, idx, sha256Hex([]byte(idx)))
	ix := static.NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatalf("RebuildTenant: %v", err)
	}
	fcas := &fakeCAS{m: map[string][]byte{
		sha256Hex([]byte("ROUTER")): []byte("ROUTER"),
		sha256Hex([]byte(idx)):      []byte(idx),
	}}

	// default: routed stack = router → reads router's own file.
	pay, _ := runReadFile(t, ix, fcas, "acme", "router", `{"files":[{"path":"own.txt","as":"o"}]}`, 1<<20)
	if got := gjson.Get(pay.Raw, "_files.o.content").String(); got != "ROUTER" {
		t.Fatalf("default routed-stack read=%q; raw=%s", got, pay.Raw)
	}

	// stack param: routed stack = router, but read the (nested) publication stack.
	pay, _ = runReadFile(t, ix, fcas, "acme", "router", `{"files":[{"path":"_data/index.json","as":"idx"}],"stack":"publications/white-fang"}`, 1<<20)
	if got := gjson.Get(pay.Raw, "_files.idx.content").String(); got != idx {
		t.Fatalf("cross-stack content=%q; raw=%s", got, pay.Raw)
	}

	// a nonexistent stack → miss (the authoritative "no such publication" signal).
	pay, _ = runReadFile(t, ix, fcas, "acme", "router", `{"files":[{"path":"_data/index.json","as":"idx"}],"stack":"publications/nope"}`, 1<<20)
	if gjson.Get(pay.Raw, "_files.idx.found").Bool() {
		t.Fatalf("nonexistent stack must miss; raw=%s", pay.Raw)
	}

	// SECURITY: the trusted pinned tenant (TenantScope) wins over a spoofed
	// envelope tenant, so `stack` can never reach another tenant's FILES.
	in := `{}`
	in, _ = sjson.Set(in, "_txc.route.tenant", "EVIL") // would mis-resolve if envelope-trusted
	in, _ = sjson.Set(in, "_txc.route.stack", "router")
	ctx := processor.WithTenant(
		operation.WithMeta(context.Background(),
			`{"files":[{"path":"_data/index.json","as":"idx"}],"stack":"publications/white-fang"}`),
		"acme")
	pay, err = readFile(ctx, ix, fcas, []byte(in), 1<<20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := gjson.Get(pay.Raw, "_files.idx.content").String(); got != idx {
		t.Fatalf("TenantScope(acme) must win over envelope tenant=EVIL; raw=%s", pay.Raw)
	}
}

func TestReadFileEncodings(t *testing.T) {
	ix := inlineIndex(t)

	// explicit base64
	pay, _ := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"robots.txt","as":"r"}],"encode":"base64"}`, 1<<20)
	g := gjson.Parse(pay.Raw)
	if got := g.Get("_files.r.encoding").String(); got != "base64" {
		t.Fatalf("encoding=%q", got)
	}
	if got := g.Get("_files.r.content").String(); got != base64.StdEncoding.EncodeToString([]byte("STACK")) {
		t.Fatalf("base64 content=%q", got)
	}

	// auto over a binary (invalid-UTF8) asset → base64
	pay, _ = runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"logo.bin","as":"logo"}]}`, 1<<20)
	g = gjson.Parse(pay.Raw)
	if got := g.Get("_files.logo.encoding").String(); got != "base64" {
		t.Fatalf("binary auto encoding=%q raw=%s", got, pay.Raw)
	}
	if got := g.Get("_files.logo.content").String(); got != base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00, 0x01}) {
		t.Fatalf("binary content=%q", got)
	}
}

func TestReadFileMissLaxVsStrict(t *testing.T) {
	ix := inlineIndex(t)

	// lax: a miss is recorded {found:false}, the op succeeds.
	pay, err := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"nope.txt","as":"n"}]}`, 1<<20)
	if err != nil {
		t.Fatalf("lax miss must not error: %v", err)
	}
	g := gjson.Parse(pay.Raw)
	if g.Get("_files.n.found").Bool() {
		t.Fatalf("missing file must be found=false; raw=%s", pay.Raw)
	}
	if g.Get("_files.n.content").Exists() {
		t.Fatalf("missing file must carry no content")
	}

	// strict: a miss fails the op.
	pay, err = runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"nope.txt","as":"n"}],"strict":true}`, 1<<20)
	if err == nil {
		t.Fatalf("strict miss must error")
	}
	if pay.Type != event.Null || !gjson.Get(pay.Meta, "error.0").Exists() {
		t.Fatalf("strict error payload shape: type=%v meta=%s", pay.Type, pay.Meta)
	}
}

func TestReadFileTruncation(t *testing.T) {
	ix := inlineIndex(t)

	// lax: over-cap truncates content but reports the original size.
	pay, err := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"robots.txt","as":"r"}]}`, 3)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	g := gjson.Parse(pay.Raw)
	if got := g.Get("_files.r.content").String(); got != "STA" {
		t.Fatalf("truncated content=%q", got)
	}
	if !g.Get("_files.r.truncated").Bool() {
		t.Fatalf("truncated flag must be set")
	}
	if got := g.Get("_files.r.size").Int(); got != int64(len("STACK")) {
		t.Fatalf("size must be original length, got %d", got)
	}

	// strict: over-cap fails.
	if _, err := runReadFile(t, ix, nil, "", "hello",
		`{"files":[{"path":"robots.txt","as":"r"}],"strict":true}`, 3); err == nil {
		t.Fatalf("strict over-cap must error")
	}
}

func TestReadFileValidation(t *testing.T) {
	ix := inlineIndex(t)
	cases := map[string]string{
		"missing files":      `{}`,
		"empty files":        `{"files":[]}`,
		"as with dot":        `{"files":[{"path":"robots.txt","as":"a.b"}]}`,
		"as with slash":      `{"files":[{"path":"robots.txt","as":"a/b"}]}`,
		"duplicate as":       `{"files":[{"path":"robots.txt","as":"r"},{"path":"robots.txt","as":"r"}]}`,
		"element missing as": `{"files":[{"path":"robots.txt"}]}`,
		"bad encode":         `{"files":[{"path":"robots.txt","as":"r"}],"encode":"rot13"}`,
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := runReadFile(t, ix, nil, "", "hello", meta, 1<<20); err == nil {
				t.Fatalf("%s must error", name)
			}
		})
	}
}

// --- tenant CAS path -------------------------------------------------------

const casDDL = `
CREATE TABLE tenants (
  tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT,
  created_at TEXT NOT NULL, revoked_at TEXT
);
CREATE TABLE stacks (
  stack_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL,
  active_version INTEGER, created_at TEXT NOT NULL
);
CREATE TABLE stack_files (
  version_id INTEGER NOT NULL, path TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '', PRIMARY KEY (version_id, path)
);`

func casIndex(t *testing.T) *static.Index {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(casDDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tenants(tenant_id,slug,name,created_at) VALUES('tnt_a','acme','acme','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stacks(stack_id,tenant_id,name,active_version,created_at) VALUES('s_a','tnt_a','web',10,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO stack_files(version_id,path,content,content_hash) VALUES(10,'FILES/_mail/welcome.html','HELLO',?)`,
		sha256Hex([]byte("HELLO"))); err != nil {
		t.Fatal(err)
	}
	ix := static.NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatalf("RebuildTenant: %v", err)
	}
	return ix
}

func TestReadFileTenantCAS(t *testing.T) {
	ix := casIndex(t)
	meta := `{"files":[{"path":"_mail/welcome.html","as":"welcome"}]}`

	// bytes present in the CAS → content resolves.
	fcas := &fakeCAS{m: map[string][]byte{sha256Hex([]byte("HELLO")): []byte("HELLO")}}
	pay, err := runReadFile(t, ix, fcas, "acme", "web", meta, 1<<20)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := gjson.Get(pay.Raw, "_files.welcome.content").String(); got != "HELLO" {
		t.Fatalf("CAS content=%q raw=%s", got, pay.Raw)
	}

	// nil store → the metadata index can lead the CAS; lax → found=false.
	pay, _ = runReadFile(t, ix, nil, "acme", "web", meta, 1<<20)
	if gjson.Get(pay.Raw, "_files.welcome.found").Bool() {
		t.Fatalf("nil CAS must yield found=false; raw=%s", pay.Raw)
	}

	// store missing the hash → lax found=false (not a hard failure).
	pay, _ = runReadFile(t, ix, &fakeCAS{m: map[string][]byte{}}, "acme", "web", meta, 1<<20)
	if gjson.Get(pay.Raw, "_files.welcome.found").Bool() {
		t.Fatalf("missing-hash CAS must yield found=false; raw=%s", pay.Raw)
	}
}
