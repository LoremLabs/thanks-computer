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

	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

func reqIn(t *testing.T, path, stack, ifNoneMatch string) []byte {
	t.Helper()
	in, _ := sjson.Set("{}", "_txc.web.req.url.path", path)
	if stack != "" {
		in, _ = sjson.Set(in, "_txc.route.stack", stack)
	}
	if ifNoneMatch != "" {
		in, _ = sjson.Set(in, "_txc.web.req.headers.If-None-Match.0", ifNoneMatch)
	}
	return []byte(in)
}

func mkfile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func body(t *testing.T, env string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(gjson.Get(env, "_txc.web.res.body").String())
	if err != nil {
		t.Fatalf("body not base64: %v", err)
	}
	return string(b)
}

// Routed-stack request gets that stack's FILES + a strong ETag and
// halts; unrouted falls back to chassis-wide.
func TestStaticResultBodyLayeredAndETag(t *testing.T) {
	ws := t.TempDir()
	mkfile(t, ws, "FILES/robots.txt", "CHASSIS")
	mkfile(t, ws, "OPS/hello-world/FILES/robots.txt", "STACK")
	ix := static.NewIndex(ws, zap.NewNop())

	env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/robots.txt", "hello-world", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 200 || !gjson.Get(env, "_txc.halt").Bool() {
		t.Fatalf("want 200+halt; env=%s", env)
	}
	if body(t, env) != "STACK" {
		t.Fatalf("routed body=%q want STACK", body(t, env))
	}
	etag := gjson.Get(env, "_txc.web.res.headers.etag.0").String()
	if len(etag) < 3 {
		t.Fatalf("missing ETag; env=%s", env)
	}

	// Conditional GET with the matching ETag → 304, no body.
	c := staticResultBody(context.Background(), ix, nil, reqIn(t, "/robots.txt", "hello-world", etag))
	if gjson.Get(c, "_txc.web.res.status").Int() != 304 {
		t.Fatalf("want 304; env=%s", c)
	}
	if gjson.Get(c, "_txc.web.res.body").Exists() {
		t.Fatalf("304 must have no body; env=%s", c)
	}
	if !gjson.Get(c, "_txc.halt").Bool() {
		t.Fatalf("304 must halt")
	}

	if body(t, staticResultBody(context.Background(), ix, nil, reqIn(t, "/robots.txt", "", ""))) != "CHASSIS" {
		t.Fatal("unrouted must fall back to chassis")
	}
}

// A directory in FILES owns its prefix: a miss under it is a static
// 404 + halt, NOT a pass-through to the app.
func TestStaticResultBodyOwnedDir404(t *testing.T) {
	ws := t.TempDir()
	mkfile(t, ws, "FILES/assets/app.css", "body{}")
	ix := static.NewIndex(ws, zap.NewNop())

	env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/assets/missing.js", "", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 404 {
		t.Fatalf("owned miss must be 404; env=%s", env)
	}
	if !gjson.Get(env, "_txc.halt").Bool() {
		t.Fatalf("owned 404 must halt; env=%s", env)
	}
}

// Embedded favicon with no workspace.
func TestStaticResultBodyEmbeddedFavicon(t *testing.T) {
	env := staticResultBody(context.Background(), static.NewIndex("", zap.NewNop()), nil, reqIn(t, "/favicon.ico", "", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 200 {
		t.Fatalf("want 200; env=%s", env)
	}
	if ct := gjson.Get(env, "_txc.web.res.headers.content-type.0").String(); ct != "image/x-icon" {
		t.Fatalf("content-type=%q", ct)
	}
}

// Missing top-level path (no owning dir) passes through unchanged.
func TestStaticResultBodyMissIsPassthrough(t *testing.T) {
	ix := static.NewIndex(t.TempDir(), zap.NewNop())
	if env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/robots.txt", "", "")); env != "{}" {
		t.Fatalf("miss must be {}; got %s", env)
	}
}

func TestStaticResultBodyTraversalRejected(t *testing.T) {
	ix := static.NewIndex(t.TempDir(), zap.NewNop())
	if env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/../server.go", "", "")); env != "{}" {
		t.Fatalf("traversal must miss; got %s", env)
	}
}

// A "_"-prefixed path segment is private: indexed (the disk index keeps it
// — safeRel only rejects "."-prefixed) but never served. It falls through
// "{}" (NOT 404) so its existence does not leak.
func TestStaticResultBodyUnderscorePrivate(t *testing.T) {
	ws := t.TempDir()
	mkfile(t, ws, "FILES/_mail/welcome.html", "<p>secret template</p>")
	mkfile(t, ws, "FILES/pub/_inner/x.txt", "nested secret")
	ix := static.NewIndex(ws, zap.NewNop())

	if env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/_mail/welcome.html", "", "")); env != "{}" {
		t.Fatalf("_-prefixed must fall through {}, got %s", env)
	}
	if env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/pub/_inner/x.txt", "", "")); env != "{}" {
		t.Fatalf("nested _-segment must fall through {}, got %s", env)
	}
	// Path normalization (/pub/../_mail/...) resolves before the check.
	if env := staticResultBody(context.Background(), ix, nil, reqIn(t, "/pub/../_mail/welcome.html", "", "")); env != "{}" {
		t.Fatalf("normalized _-segment must fall through {}, got %s", env)
	}
}

// reqInTenant builds a request envelope carrying _txc.route.tenant so the
// serve op resolves the tenant CAS layer.
func reqInTenant(t *testing.T, path, stack, tenant, ifNoneMatch string) []byte {
	t.Helper()
	in := string(reqIn(t, path, stack, ifNoneMatch))
	if tenant != "" {
		in, _ = sjson.Set(in, "_txc.route.tenant", tenant)
	}
	return []byte(in)
}

const tenantSchemaDDL = `
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE stacks (stack_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL, active_version INTEGER, created_at TEXT NOT NULL);
CREATE TABLE stack_files (version_id INTEGER NOT NULL, path TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL DEFAULT '', PRIMARY KEY (version_id, path));`

// Tenant FILES/ assets serve from the content-addressed store: the index
// holds (path → hash), the bytes come from filecas, the ETag is the hash.
func TestStaticResultBodyTenantCAS(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(tenantSchemaDDL); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants VALUES('tnt_a','acme','acme','t',NULL)`)
	exec(`INSERT INTO stacks VALUES('s_a','tnt_a','web',7,'t')`)
	content := "<h1>Acme</h1>"
	hsum := sha256.Sum256([]byte(content))
	hh := hex.EncodeToString(hsum[:])
	exec(`INSERT INTO stack_files VALUES(7,'FILES/index.html',?,?)`, content, hh)
	// A FILES row whose bytes were never written to the CAS (inconsistency).
	missing := "no bytes in cas"
	mh := sha256.Sum256([]byte(missing))
	exec(`INSERT INTO stack_files VALUES(7,'FILES/orphan.html',?,?)`, missing, hex.EncodeToString(mh[:]))

	fcas, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fcas.Put(ctx, hh, []byte(content)); err != nil { // index.html only
		t.Fatal(err)
	}

	ix := static.NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}

	env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/index.html", "web", "acme", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 200 || !gjson.Get(env, "_txc.halt").Bool() {
		t.Fatalf("want 200+halt; env=%s", env)
	}
	if body(t, env) != content {
		t.Fatalf("body=%q want %q", body(t, env), content)
	}
	etag := gjson.Get(env, "_txc.web.res.headers.etag.0").String()
	if etag != `"`+hh+`"` {
		t.Fatalf("ETag=%q want quoted content hash", etag)
	}
	if ct := gjson.Get(env, "_txc.web.res.headers.content-type.0").String(); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", ct)
	}

	// Conditional GET by the content hash → 304.
	c := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/index.html", "web", "acme", etag))
	if gjson.Get(c, "_txc.web.res.status").Int() != 304 {
		t.Fatalf("want 304; env=%s", c)
	}

	// Unknown tenant must not resolve (isolation).
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/index.html", "web", "other", "")); env != "{}" {
		t.Fatalf("unknown tenant must miss; got %s", env)
	}
	// Indexed file with bytes absent from the CAS → fall through {} (no 500).
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/orphan.html", "web", "acme", "")); env != "{}" {
		t.Fatalf("CAS miss must fall through {}; got %s", env)
	}
	// Nil store → tenant entry falls through {} (open-core embedder).
	if env := staticResultBody(ctx, ix, nil, reqInTenant(t, "/index.html", "web", "acme", "")); env != "{}" {
		t.Fatalf("nil fcas must fall through {}; got %s", env)
	}
}
