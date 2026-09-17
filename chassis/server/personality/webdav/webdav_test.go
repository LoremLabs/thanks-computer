package webdav

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/drive/filestore"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

type fakeResolver map[string]string

func (f fakeResolver) ResolveErr(k ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	t, ok := f[k.Hostname]
	return ingress.RouteTarget{Tenant: t, Stack: "core", Verified: true}, ok, nil
}

type fakeAdmission struct{ suspended string }

func (f fakeAdmission) Decide(tenant string) admission.Decision {
	if tenant == f.suspended {
		return admission.Decision{Admit: false, Status: 402, Reason: "suspended", Retry: 30 * time.Second}
	}
	return admission.Decision{Admit: true}
}
func (fakeAdmission) AllowRate(string) (bool, time.Duration)           { return true, 0 }
func (fakeAdmission) AcquireConcurrency(string, *admission.Lease) bool { return true }

type harness struct {
	ctrl  *Controller
	store *chdrive.Store
	srv   *httptest.Server
	coll  chdrive.Collection
}

func newHarness(t *testing.T, conf config.Config) *harness {
	t.Helper()
	return newHarnessWith(t, conf, nil)
}

// newHarnessWith lets a test stand a different object store behind the
// head: wrap receives the file backend and returns what the Store sees.
func newHarnessWith(t *testing.T, conf config.Config, wrap func(chdrive.ObjectStore) chdrive.ObjectStore) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "drive.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsObjects, err := filestore.New(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	var objects chdrive.ObjectStore = fsObjects
	if wrap != nil {
		objects = wrap(objects)
	}
	store := chdrive.NewStore(db, registry.SQLite, objects)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	conf.Personalities = "web,webdav"
	if conf.DrivePathPrefix == "" {
		conf.DrivePathPrefix = "/drive"
	}
	if conf.DriveLoginRate == 0 {
		conf.DriveLoginRate = 100
	}
	if conf.DriveMaxFileBytes == 0 {
		conf.DriveMaxFileBytes = 1 << 20
	}
	store.SetLimits(chdrive.Limits{MaxFileBytes: int64(conf.DriveMaxFileBytes)}) // as boot wires it
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, store, fakeResolver{"pony.example.com": "acme", "other.example.com": "other", "sad.example.com": "suspended"})
	ctrl.Start()
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(func() { srv.Close(); ctrl.Stop(); cancel() })
	h := &harness{ctrl: ctrl, store: store, srv: srv}
	h.coll, _, err = store.EnsureCollection(context.Background(), "acme", "paris")
	if err != nil {
		t.Fatal(err)
	}
	h.account(t, "acme", "paris@pony.example.com", "correct-horse", h.coll.ID)
	return h
}

func (h *harness) account(t *testing.T, tenant, username, password, collID string) {
	t.Helper()
	hash, err := apppass.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpsertAccount(context.Background(), tenant, username, hash, "", collID); err != nil {
		t.Fatal(err)
	}
}

type reqOpt func(*http.Request)

func hdr(k, v string) reqOpt     { return func(r *http.Request) { r.Header.Set(k, v) } }
func auth(u, p string) reqOpt    { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
func host(h string) reqOpt       { return func(r *http.Request) { r.Host = h } }
func noAuth() reqOpt             { return func(r *http.Request) { r.Header.Del("Authorization") } }
func contentLength(n int) reqOpt { return func(r *http.Request) { r.ContentLength = int64(n) } }
func chunked() reqOpt            { return func(r *http.Request) { r.ContentLength = -1 } }
func body(s string) io.Reader    { return strings.NewReader(s) }
func insecure(c *config.Config)  { c.DriveInsecureAuth = true }
func withPrefix(p string) reqOpt { return func(r *http.Request) { r.URL.Path = p } }

// do sends a request as the default account on the default host.
func (h *harness) do(t *testing.T, method, path string, b io.Reader, opts ...reqOpt) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, b)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "pony.example.com"
	req.SetBasicAuth("paris@pony.example.com", "correct-horse")
	if b != nil {
		if s, ok := b.(*strings.Reader); ok {
			req.ContentLength = int64(s.Len())
		}
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func want(t *testing.T, resp *http.Response, code int, what string) {
	t.Helper()
	if resp.StatusCode != code {
		t.Fatalf("%s: status %d, want %d", what, resp.StatusCode, code)
	}
}

func TestAuthGates(t *testing.T) {
	var conf config.Config
	h := newHarness(t, conf) // insecureAuth off
	resp, _ := h.do(t, "PROPFIND", "/drive/", nil)
	want(t, resp, http.StatusForbidden, "plaintext without insecure-auth")
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, hdr("X-Forwarded-Proto", "https"), hdr("Depth", "0"))
	want(t, resp, http.StatusMultiStatus, "forwarded https")

	insecure(&conf)
	h = newHarness(t, conf)
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, noAuth())
	want(t, resp, http.StatusUnauthorized, "no credentials")
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `realm="drive"`) {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, auth("paris@pony.example.com", "wrong"))
	want(t, resp, http.StatusUnauthorized, "wrong password")
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, auth("nobody@pony.example.com", "x"))
	want(t, resp, http.StatusUnauthorized, "unknown account")
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, host("other.example.com"))
	want(t, resp, http.StatusUnauthorized, "right password, wrong tenant's host")
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, host("unknown.example.com"))
	want(t, resp, http.StatusNotFound, "unrouted host")
	// Bare local part completes to the host.
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, auth("paris", "correct-horse"), hdr("Depth", "0"))
	want(t, resp, http.StatusMultiStatus, "bare local part")
	// Disabled account.
	if _, err := h.store.UpsertAccount(context.Background(), "acme", "paris@pony.example.com", "", chdrive.StatusDisabled, ""); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil)
	want(t, resp, http.StatusUnauthorized, "disabled account")
	// Suspended tenant.
	sad, _, _ := h.store.EnsureCollection(context.Background(), "suspended", "c")
	h.account(t, "suspended", "sad@sad.example.com", "correct-horse", sad.ID)
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, host("sad.example.com"), auth("sad@sad.example.com", "correct-horse"))
	want(t, resp, http.StatusPaymentRequired, "suspended tenant")
	// OPTIONS needs no credentials and advertises class 2.
	resp, _ = h.do(t, "OPTIONS", "/drive/", nil, noAuth())
	want(t, resp, http.StatusOK, "options")
	if resp.Header.Get("DAV") != "1, 2, 3" || !strings.Contains(resp.Header.Get("Allow"), "LOCK") || resp.Header.Get("MS-Author-Via") != "DAV" {
		t.Errorf("OPTIONS headers = %v", resp.Header)
	}
}

func TestThrottle(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	conf.DriveLoginRate = 3
	h := newHarness(t, conf)
	for i := 0; i < 3; i++ {
		resp, _ := h.do(t, "PROPFIND", "/drive/", nil, auth("paris@pony.example.com", "wrong"))
		want(t, resp, http.StatusUnauthorized, "wrong password")
	}
	resp, _ := h.do(t, "PROPFIND", "/drive/", nil, auth("paris@pony.example.com", "wrong"))
	want(t, resp, http.StatusTooManyRequests, "throttled")
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
}

func TestFileLifecycle(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)

	// A PUT without a Content-Length (chunked — what Finder sends for a
	// dragged file) streams and creates; then the same bytes with a length
	// are a 204 with the same ETag.
	resp, _ := h.do(t, http.MethodPut, "/drive/hello.txt", body("hello"), chunked(), hdr("Content-Type", "text/plain"))
	want(t, resp, http.StatusCreated, "chunked put")
	if resp.Header.Get("ETag") != `"`+chdrive.ETagOf([]byte("hello"))+`"` {
		t.Fatalf("chunked put ETag = %q", resp.Header.Get("ETag"))
	}
	if got, _ := h.do(t, http.MethodGet, "/drive/hello.txt", nil); got.StatusCode != http.StatusOK || got.Header.Get("Content-Length") != "5" {
		t.Fatalf("chunked put stored %s bytes", got.Header.Get("Content-Length"))
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello"), hdr("Content-Type", "text/plain"))
	want(t, resp, http.StatusNoContent, "put same with length")
	if _, err := h.store.Delete(context.Background(), h.coll.ID, "hello.txt", chdrive.DeleteOpts{}); err != nil {
		t.Fatal(err)
	}
	// A chunked body over the cap is refused after the cap, not stored.
	resp, _ = h.do(t, http.MethodPut, "/drive/toobig.bin", body(strings.Repeat("x", (1<<20)+1)), chunked())
	want(t, resp, http.StatusRequestEntityTooLarge, "chunked over cap")
	if _, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "toobig.bin"); ok {
		t.Fatal("over-cap chunked body left a row")
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello"), hdr("Content-Type", "text/plain"))
	want(t, resp, http.StatusCreated, "put create")
	etag := resp.Header.Get("ETag")
	if etag != `"`+chdrive.ETagOf([]byte("hello"))+`"` {
		t.Fatalf("ETag = %q", etag)
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello"))
	want(t, resp, http.StatusNoContent, "put same")
	// Conditional PUT.
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello2"), hdr("If-Match", `"nope"`))
	want(t, resp, http.StatusPreconditionFailed, "if-match wrong")
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello2"), hdr("If-None-Match", "*"))
	want(t, resp, http.StatusPreconditionFailed, "if-none-match star on existing")
	resp, _ = h.do(t, http.MethodPut, "/drive/hello.txt", body("hello2"), hdr("If-Match", etag))
	want(t, resp, http.StatusNoContent, "if-match right")
	etag2 := resp.Header.Get("ETag")
	if etag2 == etag {
		t.Fatal("etag did not change")
	}
	// GET / HEAD.
	resp, out := h.do(t, http.MethodGet, "/drive/hello.txt", nil)
	want(t, resp, http.StatusOK, "get")
	if out != "hello2" || resp.Header.Get("ETag") != etag2 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || resp.Header.Get("Content-Length") != "6" {
		t.Errorf("get = %q %v", out, resp.Header)
	}
	resp, out = h.do(t, http.MethodHead, "/drive/hello.txt", nil)
	want(t, resp, http.StatusOK, "head")
	if out != "" || resp.Header.Get("Content-Length") != "6" {
		t.Errorf("head = %q %v", out, resp.Header)
	}
	resp, out = h.do(t, http.MethodGet, "/drive/hello.txt", nil, hdr("Range", "bytes=1-2"))
	want(t, resp, http.StatusPartialContent, "range")
	if out != "el" {
		t.Errorf("range = %q", out)
	}
	resp, _ = h.do(t, http.MethodGet, "/drive/nope.txt", nil)
	want(t, resp, http.StatusNotFound, "get missing")
	resp, _ = h.do(t, http.MethodGet, "/drive/", nil)
	want(t, resp, http.StatusMethodNotAllowed, "get dir")
	// Too large.
	big := strings.Repeat("x", (1<<20)+1)
	resp, _ = h.do(t, http.MethodPut, "/drive/big", body(big))
	want(t, resp, http.StatusRequestEntityTooLarge, "over cap")
	// Zero-byte file, unicode + spaces.
	resp, _ = h.do(t, http.MethodPut, "/drive/empty", body(""))
	want(t, resp, http.StatusCreated, "empty put")
	resp, out = h.do(t, http.MethodGet, "/drive/empty", nil)
	want(t, resp, http.StatusOK, "empty get")
	if out != "" {
		t.Errorf("empty = %q", out)
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/caf%C3%A9%20menu.txt", body("m"))
	want(t, resp, http.StatusCreated, "unicode put")
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, hdr("Depth", "1"))
	want(t, resp, http.StatusMultiStatus, "propfind")
	// PROPFIND hrefs carry the prefix, and the root is a collection.
	resp, out = h.do(t, "PROPFIND", "/drive/", nil, hdr("Depth", "1"))
	want(t, resp, http.StatusMultiStatus, "propfind depth 1")
	// go-webdav writes the DAV: default namespace and escapes the etag's
	// quotes as &#34;.
	qetag := strings.ReplaceAll(etag2, `"`, "&#34;")
	for _, want := range []string{"<href>/drive/</href>", "<href>/drive/hello.txt</href>", "<href>/drive/caf%C3%A9%20menu.txt</href>", "<collection", ">" + qetag + "</getetag>", ">6</getcontentlength>"} {
		if !strings.Contains(out, want) {
			t.Errorf("propfind lacks %s in %s", want, out)
		}
	}
	resp, out = h.do(t, "PROPFIND", "/drive/hello.txt", nil, hdr("Depth", "0"))
	want(t, resp, http.StatusMultiStatus, "propfind file")
	if strings.Count(out, "<response") != 1 || strings.Contains(out, "<collection") {
		t.Errorf("propfind file = %s", out)
	}
	resp, _ = h.do(t, "PROPFIND", "/drive/", nil, hdr("Depth", "infinity"))
	want(t, resp, http.StatusForbidden, "propfind infinity")
	resp, _ = h.do(t, "PROPFIND", "/drive/nope", nil, hdr("Depth", "0"))
	want(t, resp, http.StatusNotFound, "propfind missing")
	// DELETE: conditional, then gone, then 404.
	resp, _ = h.do(t, http.MethodDelete, "/drive/hello.txt", nil, hdr("If-Match", `"nope"`))
	want(t, resp, http.StatusPreconditionFailed, "delete if-match")
	resp, _ = h.do(t, http.MethodDelete, "/drive/hello.txt", nil)
	want(t, resp, http.StatusNoContent, "delete")
	resp, _ = h.do(t, http.MethodDelete, "/drive/hello.txt", nil)
	want(t, resp, http.StatusNotFound, "delete twice")
	resp, _ = h.do(t, http.MethodDelete, "/drive/", nil)
	want(t, resp, http.StatusForbidden, "delete root")
}

func TestDirectoriesMoveCopy(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)

	resp, _ := h.do(t, "MKCOL", "/drive/a/b", nil)
	want(t, resp, http.StatusConflict, "mkcol without parent")
	resp, _ = h.do(t, "MKCOL", "/drive/a", nil)
	want(t, resp, http.StatusCreated, "mkcol")
	resp, _ = h.do(t, "MKCOL", "/drive/a", nil)
	want(t, resp, http.StatusMethodNotAllowed, "mkcol twice")
	resp, _ = h.do(t, "MKCOL", "/drive/a/b/", nil)
	want(t, resp, http.StatusCreated, "mkcol nested with slash")
	resp, _ = h.do(t, http.MethodPut, "/drive/a/b/f.txt", body("content"))
	want(t, resp, http.StatusCreated, "put nested")
	fres, _, _ := h.store.Stat(context.Background(), h.coll.ID, "a/b/f.txt")
	resp, _ = h.do(t, http.MethodPut, "/drive/a/x/f.txt", body("content"))
	want(t, resp, http.StatusConflict, "put without parent")
	resp, _ = h.do(t, http.MethodPut, "/drive/a", body("content"))
	want(t, resp, http.StatusForbidden, "put over collection")

	// MOVE: rename keeps the id; destination outside the mount 403; onto
	// self 403; no-overwrite over existing 412; overwrite replaces.
	resp, _ = h.do(t, "MOVE", "/drive/a/b/f.txt", nil, hdr("Destination", h.srv.URL+"/elsewhere/f.txt"))
	want(t, resp, http.StatusForbidden, "move outside prefix")
	resp, _ = h.do(t, "MOVE", "/drive/a", nil, hdr("Destination", h.srv.URL+"/drive/a/b/in"))
	want(t, resp, http.StatusForbidden, "move into self")
	resp, _ = h.do(t, "MOVE", "/drive/a/b/f.txt", nil, hdr("Destination", h.srv.URL+"/drive/g.txt"))
	want(t, resp, http.StatusCreated, "move file")
	if got, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "g.txt"); !ok || got.ResourceID != fres.ResourceID {
		t.Fatalf("moved file: %+v ok=%v", got, ok)
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/h.txt", body("other"))
	want(t, resp, http.StatusCreated, "put h")
	resp, _ = h.do(t, "MOVE", "/drive/g.txt", nil, hdr("Destination", h.srv.URL+"/drive/h.txt"), hdr("Overwrite", "F"))
	want(t, resp, http.StatusPreconditionFailed, "move no-overwrite")
	resp, _ = h.do(t, "MOVE", "/drive/g.txt", nil, hdr("Destination", h.srv.URL+"/drive/h.txt"))
	want(t, resp, http.StatusNoContent, "move overwrite")
	resp, out := h.do(t, http.MethodGet, "/drive/h.txt", nil)
	want(t, resp, http.StatusOK, "get after move")
	if out != "content" {
		t.Errorf("after move = %q", out)
	}
	// MOVE a directory with a relative Destination (what some clients send).
	resp, _ = h.do(t, http.MethodPut, "/drive/a/b/deep.txt", body("deep"))
	want(t, resp, http.StatusCreated, "put deep")
	resp, _ = h.do(t, "MOVE", "/drive/a", nil, hdr("Destination", "/drive/moved"))
	want(t, resp, http.StatusCreated, "move dir")
	resp, out = h.do(t, http.MethodGet, "/drive/moved/b/deep.txt", nil)
	want(t, resp, http.StatusOK, "get after dir move")
	if out != "deep" {
		t.Errorf("after dir move = %q", out)
	}
	resp, _ = h.do(t, "PROPFIND", "/drive/a", nil, hdr("Depth", "0"))
	want(t, resp, http.StatusNotFound, "old dir gone")
	// COPY: new id, bytes independent; Depth 0 on a collection copies an
	// empty collection.
	resp, _ = h.do(t, "COPY", "/drive/moved", nil, hdr("Destination", "/drive/copy"))
	want(t, resp, http.StatusCreated, "copy dir")
	got, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "copy/b/deep.txt")
	if !ok || got.ETag != chdrive.ETagOf([]byte("deep")) {
		t.Fatalf("copied deep: %+v ok=%v", got, ok)
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/copy/b/deep.txt", body("changed"))
	want(t, resp, http.StatusNoContent, "rewrite copy")
	resp, out = h.do(t, http.MethodGet, "/drive/moved/b/deep.txt", nil)
	want(t, resp, http.StatusOK, "source after copy rewrite")
	if out != "deep" {
		t.Errorf("source changed with copy: %q", out)
	}
	resp, _ = h.do(t, "COPY", "/drive/moved", nil, hdr("Destination", "/drive/shallow"), hdr("Depth", "0"))
	want(t, resp, http.StatusCreated, "copy depth 0")
	if rows, _ := h.store.List(context.Background(), h.coll.ID, chdrive.ListOpts{Path: "shallow"}); len(rows) != 0 {
		t.Errorf("depth-0 copy has members: %d", len(rows))
	}
	resp, _ = h.do(t, "COPY", "/drive/moved", nil, hdr("Destination", "/drive/copy"), hdr("Overwrite", "F"))
	want(t, resp, http.StatusPreconditionFailed, "copy no-overwrite")
	// DELETE a tree.
	resp, _ = h.do(t, http.MethodDelete, "/drive/moved", nil)
	want(t, resp, http.StatusNoContent, "delete tree")
	resp, _ = h.do(t, "PROPFIND", "/drive/moved/b/deep.txt", nil, hdr("Depth", "0"))
	want(t, resp, http.StatusNotFound, "deleted descendant")
	// PROPPATCH is answered (207) with every property refused.
	resp, out = h.do(t, "PROPPATCH", "/drive/h.txt",
		body(`<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:"><D:set><D:prop><D:displayname>x</D:displayname></D:prop></D:set></D:propertyupdate>`),
		hdr("Content-Type", "application/xml"))
	want(t, resp, http.StatusMultiStatus, "proppatch")
	if !strings.Contains(out, "403") {
		t.Errorf("proppatch = %s", out)
	}
}

func TestLockUnlock(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)
	lockBody := `<?xml version="1.0" encoding="utf-8"?><D:lockinfo xmlns:D="DAV:"><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype><D:owner><D:href>mailto:paris@pony.example.com</D:href></D:owner></D:lockinfo>`

	// LOCK on an unmapped URL creates an empty file (Finder's new-file
	// dance) and answers 201 with a token.
	resp, out := h.do(t, "LOCK", "/drive/new.txt", body(lockBody), hdr("Timeout", "Second-600"), hdr("Content-Type", "application/xml"))
	want(t, resp, http.StatusCreated, "lock new")
	token := resp.Header.Get("Lock-Token")
	if !strings.HasPrefix(token, "<opaquelocktoken:") || !strings.Contains(out, token[1:len(token)-1]) ||
		!strings.Contains(out, "mailto:paris@pony.example.com") || !strings.Contains(out, "<D:timeout>Second-600</D:timeout>") ||
		!strings.Contains(out, "<D:lockroot><D:href>/drive/new.txt</D:href></D:lockroot>") {
		t.Errorf("lock = %s %s", token, out)
	}
	if got, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "new.txt"); !ok || got.Size != 0 {
		t.Fatalf("lock-null: %+v ok=%v", got, ok)
	}
	// PUT, then UNLOCK: 204.
	resp, _ = h.do(t, http.MethodPut, "/drive/new.txt", body("data"), hdr("If", "("+token+")"))
	want(t, resp, http.StatusNoContent, "put under lock")
	resp, _ = h.do(t, "UNLOCK", "/drive/new.txt", nil, hdr("Lock-Token", token))
	want(t, resp, http.StatusNoContent, "unlock")
	// LOCK an existing file: 200, refresh keeps the token.
	resp, _ = h.do(t, "LOCK", "/drive/new.txt", body(lockBody), hdr("Content-Type", "application/xml"))
	want(t, resp, http.StatusOK, "lock existing")
	token = resp.Header.Get("Lock-Token")
	resp, _ = h.do(t, "LOCK", "/drive/new.txt", nil, hdr("If", "("+token+")"))
	want(t, resp, http.StatusOK, "lock refresh")
	if resp.Header.Get("Lock-Token") != token {
		t.Errorf("refresh minted a new token: %s vs %s", resp.Header.Get("Lock-Token"), token)
	}
	// LOCK on a missing parent is 409; a refresh of a missing resource 404.
	resp, _ = h.do(t, "LOCK", "/drive/nodir/x", body(lockBody), hdr("Content-Type", "application/xml"))
	want(t, resp, http.StatusConflict, "lock without parent")
	resp, _ = h.do(t, "LOCK", "/drive/gone", nil, hdr("If", "(<opaquelocktoken:abc>)"))
	want(t, resp, http.StatusNotFound, "refresh missing")
}

func TestPrefixAndAccountBinding(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	conf.DrivePathPrefix = "/files/"
	h := newHarness(t, conf)
	if h.ctrl.Prefix() != "/files" {
		t.Fatalf("prefix = %q", h.ctrl.Prefix())
	}
	resp, _ := h.do(t, http.MethodPut, "/files/a.txt", body("a"))
	want(t, resp, http.StatusCreated, "put under prefix")
	resp, out := h.do(t, "PROPFIND", "/files/", nil, hdr("Depth", "1"))
	want(t, resp, http.StatusMultiStatus, "propfind under prefix")
	if !strings.Contains(out, "<href>/files/a.txt</href>") {
		t.Errorf("hrefs = %s", out)
	}
	// A second account on another collection of the same tenant sees only
	// its own tree.
	c2, _, _ := h.store.EnsureCollection(context.Background(), "acme", "second")
	h.account(t, "acme", "two@pony.example.com", "correct-horse", c2.ID)
	resp, _ = h.do(t, http.MethodGet, "/files/a.txt", nil, auth("two@pony.example.com", "correct-horse"))
	want(t, resp, http.StatusNotFound, "other collection")
	// An account bound to a removed collection has nothing to serve.
	if _, err := h.store.DeleteCollection(context.Background(), "acme", "second", true); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(t, "PROPFIND", "/files/", nil, auth("two@pony.example.com", "correct-horse"), hdr("Depth", "0"))
	want(t, resp, http.StatusNotFound, "removed collection")
	// The store's events reflect the head's writes (one per mutation).
	if c, _, _ := h.store.GetCollectionByID(context.Background(), h.coll.ID); c.SyncToken != 1 || c.ResourceCount != 1 {
		t.Errorf("collection after head writes: %+v", c)
	}
}

func TestDisabledController(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web"}, Logger: zap.NewNop()}
	c := NewController(context.Background(), pu, nil, nil)
	if c.Enabled() {
		t.Fatal("enabled without a store")
	}
	c.Start()
	c.Stop()
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("PROPFIND", "/drive/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled head answered %d", rec.Code)
	}
	if bodyBudget(0) != bodyFloor || bodyBudget(10<<20) != bodyFloor+10*bodyPerMiB || bodyBudget(1<<40) != bodyCeil {
		t.Error("bodyBudget")
	}
}

// TestCollectionPolicy: a collection can reserve a subtree for the stack.
// The case it exists for is a curated folder — the stack puts documents in
// Knowledge/ and a desktop client must not overwrite them, which is how a
// client with a stale cache silently replaced a good file with a spliced
// one (2026-09-17). Everything that does not move bytes stays available,
// because the person still files and forgets things in that tree.
func TestCollectionPolicy(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)
	ctx := context.Background()

	// Seed the tree as the stack would, BEFORE the policy is in force.
	for _, p := range []string{"Knowledge", "Knowledge/ai", "Input"} {
		if _, err := h.store.Mkdir(ctx, h.coll.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"Knowledge/ai/paper.pdf", "Knowledge/ai/keep.pdf"} {
		if _, err := h.store.Put(ctx, h.coll.ID, p, strings.NewReader("good"), 4, chdrive.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.SetCollectionPolicy(ctx, "acme", "paris", chdrive.Policy{
		"Knowledge": {chdrive.VerbWrite: chdrive.ModeDeny},
	}); err != nil {
		t.Fatal(err)
	}

	// The refused verb: a client cannot write bytes into the curated tree,
	// at any depth, whether it would create or overwrite.
	resp, _ := h.do(t, http.MethodPut, "/drive/Knowledge/ai/paper.pdf", body("spliced"))
	want(t, resp, http.StatusForbidden, "overwrite in a reserved tree")
	resp, _ = h.do(t, http.MethodPut, "/drive/Knowledge/new.txt", body("x"))
	want(t, resp, http.StatusForbidden, "create in a reserved tree")
	// And the bytes really are untouched.
	if got, _, err := h.store.Open(ctx, h.coll.ID, "Knowledge/ai/paper.pdf"); err != nil {
		t.Fatal(err)
	} else {
		b, _ := io.ReadAll(got)
		got.Close()
		if string(b) != "good" {
			t.Fatalf("the refused PUT still changed the file: %q", b)
		}
	}

	// The drop zone is untouched by the policy: that is where a person works.
	resp, _ = h.do(t, http.MethodPut, "/drive/Input/drop.txt", body("mine"))
	want(t, resp, http.StatusCreated, "write outside the reserved tree")

	// Client bookkeeping is exempt, or browsing a read-only folder in
	// Finder becomes a stream of error dialogs.
	resp, _ = h.do(t, http.MethodPut, "/drive/Knowledge/ai/._paper.pdf", body("applesingle"))
	want(t, resp, http.StatusCreated, "AppleDouble sidecar in a reserved tree")
	resp, _ = h.do(t, http.MethodPut, "/drive/Knowledge/.DS_Store", body("finder"))
	want(t, resp, http.StatusCreated, ".DS_Store in a reserved tree")

	// What does NOT move bytes stays the person's: making a folder (the
	// taxonomy the stack files INTO), filing, and forgetting.
	resp, _ = h.do(t, "MKCOL", "/drive/Knowledge/people", nil)
	want(t, resp, http.StatusCreated, "mkcol in a reserved tree")
	resp, _ = h.do(t, "MOVE", "/drive/Knowledge/ai/paper.pdf", nil, hdr("Destination", h.srv.URL+"/drive/Knowledge/people/paper.pdf"))
	want(t, resp, http.StatusCreated, "file within a reserved tree")
	resp, _ = h.do(t, http.MethodDelete, "/drive/Knowledge/people/paper.pdf", nil)
	want(t, resp, http.StatusNoContent, "delete from a reserved tree")

	// Reading is never affected.
	resp, _ = h.do(t, "PROPFIND", "/drive/Knowledge/", nil, hdr("Depth", "1"))
	want(t, resp, http.StatusMultiStatus, "propfind a reserved tree")
	resp, _ = h.do(t, http.MethodGet, "/drive/Knowledge/ai/keep.pdf", nil)
	want(t, resp, http.StatusOK, "get from a reserved tree")

	// A LOCK is a WRITE lock, so it is refused in the reserved tree — and
	// refused UP FRONT, which is the point: a client that cannot take a
	// write lock opens the file read-only instead of discovering at save
	// time that it cannot save, and then retrying until the mount stalls.
	resp, _ = h.do(t, "LOCK", "/drive/Knowledge/ai/keep.pdf", nil)
	want(t, resp, http.StatusForbidden, "lock in a reserved tree")
	// A lock on an unmapped URL would CREATE the file, so it is refused too
	// (and must not leave the empty file behind).
	resp, _ = h.do(t, "LOCK", "/drive/Knowledge/ghost.pdf", nil)
	want(t, resp, http.StatusForbidden, "lock creating in a reserved tree")
	if _, ok, _ := h.store.Stat(ctx, h.coll.ID, "Knowledge/ghost.pdf"); ok {
		t.Fatal("a refused LOCK still created the file")
	}
	// Outside the reserved tree, and for client bookkeeping inside it,
	// locking is untouched.
	resp, _ = h.do(t, "LOCK", "/drive/Input/drop.txt", nil)
	want(t, resp, http.StatusOK, "lock outside the reserved tree")
	// Finder locks a sidecar before its first PUT, so this one is a create
	// (201) — and the exemption has to cover that, not just the overwrite.
	resp, _ = h.do(t, "LOCK", "/drive/Knowledge/ai/._keep.pdf", nil)
	want(t, resp, http.StatusCreated, "lock an AppleDouble sidecar")

	// The stack itself is never subject to the policy — that asymmetry is
	// the whole point, and it is what keeps ingest able to place a file.
	if _, err := h.store.Put(ctx, h.coll.ID, "Knowledge/ai/stack.pdf", strings.NewReader("by the stack"), 12, chdrive.PutOpts{}); err != nil {
		t.Fatalf("the policy leaked into the store: %v", err)
	}

	// A tighter policy can refuse the rest; a cleared one restores an
	// ordinary drive.
	if _, err := h.store.SetCollectionPolicy(ctx, "acme", "paris", chdrive.Policy{
		"Knowledge": {chdrive.VerbWrite: chdrive.ModeDeny, chdrive.VerbDelete: chdrive.ModeDeny, chdrive.VerbCreate: chdrive.ModeDeny},
	}); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(t, "MKCOL", "/drive/Knowledge/nope", nil)
	want(t, resp, http.StatusForbidden, "mkcol under a stricter policy")
	resp, _ = h.do(t, http.MethodDelete, "/drive/Knowledge/ai/stack.pdf", nil)
	want(t, resp, http.StatusForbidden, "delete under a stricter policy")
	if _, err := h.store.SetCollectionPolicy(ctx, "acme", "paris", nil); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(t, http.MethodPut, "/drive/Knowledge/ai/paper.pdf", body("now allowed"))
	want(t, resp, http.StatusCreated, "write after the policy is cleared")
}

// TestNamedMount: the same collection answers at the bare prefix and at a
// prefix that names it, so a client can mount `/drive/paris/` and show the
// volume as "paris" instead of a third "drive". Whichever form the request
// used comes back in the hrefs, or a client would follow a listing out of
// its own mount.
func TestNamedMount(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)

	resp, _ := h.do(t, http.MethodPut, "/drive/paris/note.txt", body("hello"))
	want(t, resp, http.StatusCreated, "put through the named mount")
	// The same file, addressed both ways.
	if got, _ := h.do(t, http.MethodGet, "/drive/note.txt", nil); got.StatusCode != http.StatusOK {
		t.Fatalf("bare mount cannot see the named mount's file: %d", got.StatusCode)
	}
	if got, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "note.txt"); !ok || got.Size != 5 {
		t.Fatalf("the named segment was stored as a folder: %+v ok=%v", got, ok)
	}

	// Hrefs come back in the form the request used.
	_, bare := h.do(t, "PROPFIND", "/drive/", nil, hdr("Depth", "1"))
	if !strings.Contains(bare, "<href>/drive/note.txt</href>") || strings.Contains(bare, "/drive/paris/note.txt") {
		t.Fatalf("bare PROPFIND hrefs: %s", bare)
	}
	_, named := h.do(t, "PROPFIND", "/drive/paris/", nil, hdr("Depth", "1"))
	if !strings.Contains(named, "<href>/drive/paris/note.txt</href>") {
		t.Fatalf("named PROPFIND hrefs: %s", named)
	}

	// A MOVE inside the named mount stays inside it.
	resp, _ = h.do(t, "MOVE", "/drive/paris/note.txt", nil, hdr("Destination", h.srv.URL+"/drive/paris/moved.txt"))
	want(t, resp, http.StatusCreated, "move within the named mount")
	if _, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "moved.txt"); !ok {
		t.Fatal("the move did not land at the collection root")
	}
	// Crossing between the two forms is still the same mount, not an escape.
	resp, _ = h.do(t, "MOVE", "/drive/moved.txt", nil, hdr("Destination", h.srv.URL+"/drive/back.txt"))
	want(t, resp, http.StatusCreated, "move within the bare mount")

	// Another collection's name is not a mount: it is an ordinary path.
	resp, _ = h.do(t, http.MethodPut, "/drive/lisbon/x.txt", body("x"))
	want(t, resp, http.StatusConflict, "a name that is not this collection has no parent")

	// A directory that shares the collection's name is reached one level in.
	resp, _ = h.do(t, "MKCOL", "/drive/paris/paris", nil)
	want(t, resp, http.StatusCreated, "a folder named like the collection")
	if _, ok, _ := h.store.Stat(context.Background(), h.coll.ID, "paris"); !ok {
		t.Fatal("the folder did not land at the collection root")
	}
}

// Conditional headers are the fleet's real concurrency control: the locks
// are a stateless shim (see lock.go), so If-Match on the etag is what
// actually stops a lost update. This walks the header forms RFC 7232
// allows, end to end through the head and the store's transaction.
func TestConditionalHeaders(t *testing.T) {
	var conf config.Config
	insecure(&conf)
	h := newHarness(t, conf)

	etagOf := func(resp *http.Response) string {
		t.Helper()
		e := strings.Trim(resp.Header.Get("ETag"), `"`)
		if e == "" {
			t.Fatal("the response carried no ETag")
		}
		return e
	}

	resp, _ := h.do(t, http.MethodPut, "/drive/doc.txt", body("one"))
	want(t, resp, http.StatusCreated, "seed")
	etag := etagOf(resp)

	// If-Match, single: the ordinary lost-update guard.
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("two"), hdr("If-Match", `"`+etag+`"`))
	want(t, resp, http.StatusNoContent, "if-match on the current etag")
	etag = etagOf(resp)
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("three"), hdr("If-Match", `"stale"`))
	want(t, resp, http.StatusPreconditionFailed, "if-match on a stale etag")

	// If-Match, list: any entry satisfies it. Trimming quotes off the whole
	// header value made this a spurious 412.
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("four"), hdr("If-Match", `"stale", "`+etag+`"`))
	want(t, resp, http.StatusNoContent, "if-match on a list holding the current etag")
	etag = etagOf(resp)
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("five"), hdr("If-Match", `"stale", "older"`))
	want(t, resp, http.StatusPreconditionFailed, "if-match on a list holding neither")

	// If-Match compares strongly, so a weak entry cannot satisfy it even
	// when the etag is right.
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("six"), hdr("If-Match", `W/"`+etag+`"`))
	want(t, resp, http.StatusPreconditionFailed, "if-match on a weak etag")

	// If-None-Match: "*" is create-only, and a list that names the live
	// etag refuses. That list is the case that used to pass silently — the
	// guard was armed and did nothing.
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("seven"), hdr("If-None-Match", "*"))
	want(t, resp, http.StatusPreconditionFailed, "create-only over a live file")
	resp, _ = h.do(t, http.MethodPut, "/drive/fresh.txt", body("new"), hdr("If-None-Match", "*"))
	want(t, resp, http.StatusCreated, "create-only over nothing")
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("eight"), hdr("If-None-Match", `"stale", "`+etag+`"`))
	want(t, resp, http.StatusPreconditionFailed, "if-none-match on a list holding the current etag")
	resp, _ = h.do(t, http.MethodPut, "/drive/doc.txt", body("nine"), hdr("If-None-Match", `"stale", "older"`))
	want(t, resp, http.StatusNoContent, "if-none-match on a list holding neither")
	etag = etagOf(resp)

	// A conditional GET: the library's ServeContent answers the revalidation.
	resp, _ = h.do(t, http.MethodGet, "/drive/doc.txt", nil, hdr("If-None-Match", `"`+etag+`"`))
	want(t, resp, http.StatusNotModified, "conditional get on the current etag")

	// DELETE carries them too, and the store applies them in the same
	// transaction as the delete rather than in a racy pre-check.
	resp, _ = h.do(t, http.MethodDelete, "/drive/doc.txt", nil, hdr("If-Match", `"stale"`))
	want(t, resp, http.StatusPreconditionFailed, "delete on a stale etag")
	resp, _ = h.do(t, http.MethodDelete, "/drive/doc.txt", nil, hdr("If-None-Match", `"`+etag+`"`))
	want(t, resp, http.StatusPreconditionFailed, "delete refused by if-none-match")
	resp, _ = h.do(t, http.MethodDelete, "/drive/doc.txt", nil, hdr("If-Match", `"stale", "`+etag+`"`))
	want(t, resp, http.StatusNoContent, "delete on a list holding the current etag")
}

// streamOnly hides everything but Read and Close on what Get returns. That
// is the shape of an S3 body — what prod serves — and what the file backend
// in this harness would otherwise paper over with an *os.File that seeks.
// Prod ignored every Range request for exactly this reason (2026-09-17):
// the library only serves ranges from a seeker.
type streamOnly struct{ chdrive.ObjectStore }

func (s streamOnly) Get(ctx context.Context, key string) (io.ReadCloser, chdrive.ObjectInfo, error) {
	rc, info, err := s.ObjectStore.Get(ctx, key)
	if err != nil {
		return nil, info, err
	}
	return struct {
		io.Reader
		io.Closer
	}{rc, rc}, info, nil
}

// ranged is streamOnly plus GetRange, recording every range asked for, so
// a test can prove the head fetched the slice rather than the object.
type ranged struct {
	streamOnly
	asked *[]string
}

func (r ranged) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	*r.asked = append(*r.asked, fmt.Sprintf("%d+%d", off, length))
	rc, _, err := r.streamOnly.ObjectStore.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if _, err := rc.(io.Seeker).Seek(off, io.SeekStart); err != nil {
		rc.Close()
		return nil, err
	}
	var rd io.Reader = rc
	if length >= 0 {
		rd = io.LimitReader(rc, length)
	}
	return struct {
		io.Reader
		io.Closer
	}{rd, rc}, nil
}

// A PDF reader finds the cross-reference table by seeking to the tail, and
// macOS webdavfs turns that seek into a Range GET. Answering 200 with the
// whole file made the client take the file's HEAD as its tail: Preview
// reported every PDF on the drive as damaged while `shasum` said the bytes
// were fine (prod, 2026-09-17). Ranges have to work on every backend, not
// only the one whose Get happens to return a seeker.
func TestGetRanges(t *testing.T) {
	content := strings.Repeat("0123456789", 1000) // 10,000 bytes, offset-legible
	for _, tc := range []struct {
		name  string
		wrap  func(chdrive.ObjectStore) chdrive.ObjectStore
		asked *[]string
	}{
		{name: "stream-only backend", wrap: func(o chdrive.ObjectStore) chdrive.ObjectStore { return streamOnly{o} }},
		{name: "ranged backend", asked: new([]string)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.asked != nil {
				tc.wrap = func(o chdrive.ObjectStore) chdrive.ObjectStore { return ranged{streamOnly{o}, tc.asked} }
			}
			var conf config.Config
			insecure(&conf)
			h := newHarnessWith(t, conf, tc.wrap)
			resp, _ := h.do(t, http.MethodPut, "/drive/big.bin", body(content))
			want(t, resp, http.StatusCreated, "seed")
			etag := resp.Header.Get("ETag")
			askedSoFar := func() int {
				if tc.asked == nil {
					return 0
				}
				return len(*tc.asked)
			}
			lastAsked := func(t *testing.T, wantPrefix string) {
				t.Helper()
				if tc.asked == nil {
					return
				}
				if n := len(*tc.asked); n == 0 || !strings.HasPrefix((*tc.asked)[n-1], wantPrefix) {
					t.Fatalf("backend was asked %v, want the last to start %q", *tc.asked, wantPrefix)
				}
			}

			// HEAD says the size and that ranges are welcome.
			resp, got := h.do(t, http.MethodHead, "/drive/big.bin", nil)
			want(t, resp, http.StatusOK, "head")
			if resp.ContentLength != int64(len(content)) || got != "" {
				t.Fatalf("head: Content-Length %d body %d bytes", resp.ContentLength, len(got))
			}
			if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
				t.Fatalf("head: Accept-Ranges %q, want bytes", ar)
			}

			// A plain GET is the whole file, and never a ranged read.
			n := askedSoFar()
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil)
			want(t, resp, http.StatusOK, "plain get")
			if got != content || resp.ContentLength != int64(len(content)) {
				t.Fatalf("plain get: %d bytes, Content-Length %d", len(got), resp.ContentLength)
			}
			if askedSoFar() != n {
				t.Fatalf("a plain GET went through GetRange: %v", *tc.asked)
			}

			// A slice from the middle.
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=5000-5009"))
			want(t, resp, http.StatusPartialContent, "range in the middle")
			if got != content[5000:5010] {
				t.Fatalf("range: got %q", got)
			}
			if cr := resp.Header.Get("Content-Range"); cr != "bytes 5000-5009/10000" {
				t.Fatalf("range: Content-Range %q", cr)
			}
			if resp.ContentLength != 10 {
				t.Fatalf("range: Content-Length %d", resp.ContentLength)
			}
			lastAsked(t, "5000+")

			// The last page, straddling EOF — what a PDF reader asks for.
			// Clamped to the file, never padded, never the whole file.
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=9990-19999"))
			want(t, resp, http.StatusPartialContent, "range straddling EOF")
			if got != content[9990:] {
				t.Fatalf("straddle: got %d bytes starting %q", len(got), got[:min(len(got), 12)])
			}
			if cr := resp.Header.Get("Content-Range"); cr != "bytes 9990-9999/10000" {
				t.Fatalf("straddle: Content-Range %q", cr)
			}
			lastAsked(t, "9990+")

			// A suffix.
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=-7"))
			want(t, resp, http.StatusPartialContent, "suffix range")
			if got != content[9993:] {
				t.Fatalf("suffix: got %q", got)
			}
			lastAsked(t, "9993+")

			// Past the end is unsatisfiable, and says how long the file is.
			n = askedSoFar()
			resp, _ = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=10000-10010"))
			want(t, resp, http.StatusRequestedRangeNotSatisfiable, "range past EOF")
			if cr := resp.Header.Get("Content-Range"); cr != "bytes */10000" {
				t.Fatalf("past EOF: Content-Range %q", cr)
			}
			if askedSoFar() != n {
				t.Fatalf("an unsatisfiable range still read the object: %v", *tc.asked)
			}

			// If-Range: the slice when the etag still holds, the whole file
			// when it does not — a client resuming into a changed file must
			// not splice two versions.
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=0-9"), hdr("If-Range", etag))
			want(t, resp, http.StatusPartialContent, "if-range, current etag")
			if got != content[:10] {
				t.Fatalf("if-range current: got %q", got)
			}
			resp, got = h.do(t, http.MethodGet, "/drive/big.bin", nil, hdr("Range", "bytes=0-9"), hdr("If-Range", `"stale"`))
			want(t, resp, http.StatusOK, "if-range, stale etag")
			if got != content {
				t.Fatalf("if-range stale: %d bytes", len(got))
			}
		})
	}
}
