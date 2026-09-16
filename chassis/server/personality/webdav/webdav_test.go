package webdav

import (
	"context"
	"database/sql"
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
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "drive.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	objects, err := filestore.New(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
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
