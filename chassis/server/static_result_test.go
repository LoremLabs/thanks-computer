package server

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

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

	env := staticResultBody(ix, reqIn(t, "/robots.txt", "hello-world", ""))
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
	c := staticResultBody(ix, reqIn(t, "/robots.txt", "hello-world", etag))
	if gjson.Get(c, "_txc.web.res.status").Int() != 304 {
		t.Fatalf("want 304; env=%s", c)
	}
	if gjson.Get(c, "_txc.web.res.body").Exists() {
		t.Fatalf("304 must have no body; env=%s", c)
	}
	if !gjson.Get(c, "_txc.halt").Bool() {
		t.Fatalf("304 must halt")
	}

	if body(t, staticResultBody(ix, reqIn(t, "/robots.txt", "", ""))) != "CHASSIS" {
		t.Fatal("unrouted must fall back to chassis")
	}
}

// A directory in FILES owns its prefix: a miss under it is a static
// 404 + halt, NOT a pass-through to the app.
func TestStaticResultBodyOwnedDir404(t *testing.T) {
	ws := t.TempDir()
	mkfile(t, ws, "FILES/assets/app.css", "body{}")
	ix := static.NewIndex(ws, zap.NewNop())

	env := staticResultBody(ix, reqIn(t, "/assets/missing.js", "", ""))
	if gjson.Get(env, "_txc.web.res.status").Int() != 404 {
		t.Fatalf("owned miss must be 404; env=%s", env)
	}
	if !gjson.Get(env, "_txc.halt").Bool() {
		t.Fatalf("owned 404 must halt; env=%s", env)
	}
}

// Embedded favicon with no workspace.
func TestStaticResultBodyEmbeddedFavicon(t *testing.T) {
	env := staticResultBody(static.NewIndex("", zap.NewNop()), reqIn(t, "/favicon.ico", "", ""))
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
	if env := staticResultBody(ix, reqIn(t, "/robots.txt", "", "")); env != "{}" {
		t.Fatalf("miss must be {}; got %s", env)
	}
}

func TestStaticResultBodyTraversalRejected(t *testing.T) {
	ix := static.NewIndex(t.TempDir(), zap.NewNop())
	if env := staticResultBody(ix, reqIn(t, "/../server.go", "", "")); env != "{}" {
		t.Fatalf("traversal must miss; got %s", env)
	}
}
