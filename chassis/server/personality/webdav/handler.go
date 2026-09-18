package webdav

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/emersion/go-webdav"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// principal is the authenticated account on a request's context, with the
// one collection it sees.
type principal struct {
	tenant   string
	username string
	acct     chdrive.Account
	coll     chdrive.Collection
	clientIP string
	// prefix is the mount prefix this REQUEST used — the bare one, or the
	// one that names the collection (mountPrefix). Every rel/href on the
	// request reads it, so a session's URLs stay in the form the client
	// mounted with.
	prefix string
}

type ctxKeyPrincipal struct{}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal{}).(principal)
	return p, ok
}

// ServeHTTP is the whole request flow. Every branch is answered from the
// store, here or by the WebDAV library over fs.go; none touches the bus.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !c.Enabled() {
		http.NotFound(w, r)
		return
	}
	// 1. Hostname → tenant: the routing every web request uses. A
	// transient resolver failure is an honest 503, never a 404.
	tenant, ok, err := c.resolveHost(r.Host)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "routing temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	// 2. OPTIONS is answered before authentication: it names no resource
	// and carries no credential, and macOS Finder / Windows send it first
	// to learn the DAV class. The library would advertise class 1 and 3
	// only; Finder mounts a class-1 server read-only, so this head says
	// 1, 2, 3 and lists LOCK/UNLOCK, which lock.go answers.
	if r.Method == http.MethodOptions {
		w.Header().Set("DAV", "1, 2, 3")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK")
		w.Header().Set("MS-Author-Via", "DAV")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	// 3. Everything else is Basic-authenticated over TLS.
	pr, ok := c.authenticate(w, r, tenant)
	if !ok {
		return
	}
	ctx := context.WithValue(r.Context(), ctxKeyPrincipal{}, pr)
	r = r.WithContext(ctx)
	// 4. What the library cannot do at v0.7.0, answered here.
	switch r.Method {
	case "LOCK":
		c.serveLock(w, r, pr)
		return
	case "UNLOCK":
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPut:
		c.servePut(w, r, pr)
		return
	case http.MethodGet, http.MethodHead:
		// Escape the web listener's global write timeout for a large body:
		// budget the deadline from the file's size before the library
		// streams it.
		if res, found, err := c.store.Stat(ctx, pr.coll.ID, c.rel(pr, r.URL.Path)); err == nil && found && !res.IsDir() {
			_ = http.NewResponseController(w).SetWriteDeadline(c.now().Add(bodyBudget(res.Size)))
		}
	case "PROPFIND":
		// RFC 4918 lets a server refuse Depth: infinity (it is the whole
		// tree in one response); an absent Depth means infinity, which a
		// bounded server treats as 1.
		switch strings.ToLower(strings.TrimSpace(r.Header.Get("Depth"))) {
		case "":
			r.Header.Set("Depth", "1")
		case "infinity":
			http.Error(w, "propfind-finite-depth", http.StatusForbidden)
			return
		}
	}
	h := webdav.Handler{FileSystem: &fs{c: c, pr: pr}}
	h.ServeHTTP(w, r)
}

// mountPrefix is the prefix IN FORCE for one request. A drive answers at
// two URLs, and the difference is cosmetic but useful:
//
//	https://host/drive/            the bare mount
//	https://host/drive/paris/      the same collection, named
//
// A client takes the volume's name from the last path segment, so the bare
// form mounts as "drive" on every pony — mount two and you get "drive" and
// "drive 1". The named form mounts as "paris". Both address the same tree;
// which one a request used decides the hrefs it gets back, so a session
// stays self-consistent.
//
// The named form wins when it matches, which means a top-level directory
// with the SAME NAME as the collection is addressed one level down
// (`/drive/paris/paris`) rather than at `/drive/paris`. That is the whole
// cost of the alias, and it is why the segment must equal the collection's
// own name rather than being any leading segment.
func (c *Controller) mountPrefix(coll chdrive.Collection, urlPath string) string {
	named := c.prefix + "/" + coll.Name
	if urlPath == named || strings.HasPrefix(urlPath, named+"/") {
		return named
	}
	return c.prefix
}

// rel strips the mount prefix from a request path; the result is what the
// drive store normalizes ("" is the root).
func (c *Controller) rel(pr principal, p string) string {
	return strings.TrimPrefix(p, pr.prefix)
}

// href is the absolute path a resource is served at, in the same form the
// request used (collections carry a trailing slash, per RFC 4918).
func (c *Controller) href(pr principal, res chdrive.Resource) string {
	if res.Path == "" {
		return pr.prefix + "/"
	}
	if res.IsDir() {
		return pr.prefix + "/" + res.Path + "/"
	}
	return pr.prefix + "/" + res.Path
}

func (c *Controller) resolveHost(host string) (string, bool, error) {
	if c.resolver == nil {
		return "", false, nil
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	t, ok, err := c.resolver.ResolveErr(ingress.RouteKey{Src: "http", Hostname: host})
	if err != nil || !ok {
		return "", ok, err
	}
	return t.Tenant, true, nil
}

// hostOnly strips a port from a Host header value.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (c *Controller) secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// authenticate is Basic auth over the account table: TLS first (before a
// credential is read), then the verified-login cache, the throttles on a
// miss only, argon2id, status, admission, the tenant match, and finally
// the account's collection. The flow is the calendar head's.
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request, tenant string) (principal, bool) {
	ip := clientIP(r)
	if !c.secure(r) && !c.insecureAuth {
		http.Error(w, "TLS required", http.StatusForbidden)
		return principal{}, false
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="drive", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return principal{}, false
	}
	// A bare local part completes to the request's host: a mount dialog
	// shows the address's local part as the "user name" and a person types
	// just that, and the head serves the address's own domain. A completed
	// name that names no account fails exactly like any other.
	if !strings.Contains(user, "@") {
		user = user + "@" + hostOnly(r.Host)
	}
	username := chdrive.NormalizeUsername(user)
	deny := func(outcome string) (principal, bool) {
		c.noteLogin(outcome, username, ip)
		w.Header().Set("WWW-Authenticate", `Basic realm="drive", charset="UTF-8"`)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return principal{}, false
	}
	throttled := func() bool {
		if c.loginIP != nil && ip != "" {
			if ok, _ := c.loginIP.Allow(ip); !ok {
				return true
			}
		}
		if c.loginAcct != nil {
			if ok, _ := c.loginAcct.Allow(username); !ok {
				return true
			}
		}
		return false
	}
	tooMany := func() (principal, bool) {
		c.noteLogin("throttled", username, ip)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return principal{}, false
	}
	acct, found, err := c.store.GetAccount(r.Context(), username)
	if err != nil {
		c.pu.Logger.Warn("webdav login lookup failed", zap.String("user", username), zap.String("err", err.Error()))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return principal{}, false
	}
	if !found {
		if throttled() {
			return tooMany()
		}
		apppass.VerifyDummy(pass)
		return deny("failed")
	}
	key := apppass.LoginKey(acct.Username, acct.PwHash, pass)
	cached := c.cache.Hit(key)
	if !cached {
		if throttled() {
			return tooMany()
		}
		match, verr := apppass.VerifyPassword(acct.PwHash, pass)
		if verr != nil {
			c.pu.Logger.Warn("webdav account has an unreadable password hash", zap.String("user", username), zap.String("err", verr.Error()))
			return deny("error")
		}
		if !match {
			return deny("failed")
		}
		c.cache.Put(key)
	}
	if acct.Status != chdrive.StatusActive {
		return deny("disabled")
	}
	if c.pu.Admission != nil {
		if d := c.pu.Admission.Decide(acct.Tenant); !d.Admit {
			c.noteLogin("denied", username, ip)
			status := d.Status
			if status == 0 {
				status = http.StatusForbidden
			}
			if d.Retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.Retry.Seconds())))
			}
			http.Error(w, "service unavailable for this account", status)
			return principal{}, false
		}
	}
	if acct.Tenant != tenant {
		// The account exists but not on this hostname's tenant: the same
		// answer as a wrong password, so nothing is learned.
		return deny("wrong_tenant")
	}
	coll, found, err := c.store.GetCollectionByID(r.Context(), acct.CollectionID)
	if err != nil {
		c.pu.Logger.Warn("webdav collection lookup failed", zap.String("user", username), zap.String("err", err.Error()))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return principal{}, false
	}
	if !found || coll.Tenant != tenant {
		// Bound to a collection that no longer exists: nothing to serve.
		c.noteLogin("no_collection", username, ip)
		http.NotFound(w, r)
		return principal{}, false
	}
	if cached {
		// Every request carries Basic auth, and a mounted drive never stops
		// asking (a PROPFIND keepalive, an app's retried save), so a hit is
		// the same client's next request, not a new login. It is counted;
		// the line is logged once per password check, i.e. once per cache
		// TTL per node. Refusals above log every time.
		c.countLogin("ok")
	} else {
		c.noteLogin("ok", username, ip)
	}
	return principal{
		tenant: tenant, username: acct.Username, acct: acct, coll: coll, clientIP: ip,
		prefix: c.mountPrefix(coll, r.URL.Path),
	}, true
}

// servePut streams a PUT body into the store. A Content-Length is used
// when the client sends one (a 413 before any byte moves, a deadline sized
// to it); a body without one — macOS Finder streams a dragged file with
// chunked encoding (prod, 2026-09-16: every such PUT was refused with 411
// and the file stayed the empty one the client had created first) — is
// streamed up to the per-file cap, with the cap's deadline. Either way the
// read deadline escapes the listener's global timeout while an abandoned
// stream is still reaped, and the body reader refuses to deliver more than
// allowed.
func (c *Controller) servePut(w http.ResponseWriter, r *http.Request, pr principal) {
	// The collection's policy decides BEFORE a byte is read: a refused write
	// must not spend the upload, and the client must not be left half-sent
	// wondering why.
	rel := c.rel(pr, r.URL.Path)
	if !c.allows(pr, rel, chdrive.VerbWrite) {
		http.Error(w, "forbidden by the collection's policy", http.StatusForbidden)
		return
	}
	size := r.ContentLength
	if size < 0 {
		size = -1
	}
	if size >= 0 && c.maxBytes > 0 && size > c.maxBytes {
		http.Error(w, fmt.Sprintf("file over %d bytes", c.maxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	budget, limit := size, size
	if size < 0 {
		// One byte past the cap, so the store sees the overflow and answers
		// ErrTooLarge (413) itself; the reader is the belt, not the judge.
		budget, limit = c.maxBytes, c.maxBytes+1
	}
	_ = http.NewResponseController(w).SetReadDeadline(c.now().Add(bodyBudget(budget)))
	body := r.Body
	if limit > 0 {
		body = http.MaxBytesReader(w, r.Body, limit)
	}
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/octet-stream" {
		ct = "" // the generic default: let the extension decide
	}
	res, err := c.store.Put(r.Context(), pr.coll.ID, c.rel(pr, r.URL.Path), body, size, chdrive.PutOpts{
		IfMatch:     r.Header.Get("If-Match"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
		ContentType: ct,
	})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("file over %d bytes", c.maxBytes), http.StatusRequestEntityTooLarge)
			return
		}
		serveStoreError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+res.ETag+`"`)
	if res.Created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

// storeStatus maps a drive error to the WebDAV status it answers with.
func storeStatus(err error) int {
	switch {
	case errors.Is(err, chdrive.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, chdrive.ErrNoParent), errors.Is(err, chdrive.ErrNotDirectory):
		return http.StatusConflict
	case errors.Is(err, chdrive.ErrPrecondition), errors.Is(err, chdrive.ErrExists):
		return http.StatusPreconditionFailed
	case errors.Is(err, chdrive.ErrQuota):
		return http.StatusInsufficientStorage
	case errors.Is(err, chdrive.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, chdrive.ErrIsDirectory), errors.Is(err, chdrive.ErrCycle):
		return http.StatusForbidden
	case errors.Is(err, chdrive.ErrBadPath), errors.Is(err, chdrive.ErrSizeMismatch):
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

func serveStoreError(w http.ResponseWriter, err error) {
	code := storeStatus(err)
	msg := err.Error()
	if code == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
		msg = "store temporarily unavailable"
	}
	http.Error(w, msg, code)
}
