package git

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// repoNameRE is the charset a repository name may have: the same as a
// credential scope's instance (chassis/authn/scope.go), because the name IS
// the instance of the `git:<repo>:fetch|push` scopes, so no `/`.
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// request is what one HTTP request resolved to before a service runs.
type request struct {
	tenant  string
	name    string // the repository, without .git
	service string // "info/refs" | git-upload-pack | git-receive-pack
	action  string // "fetch" | "push": the credential scope the request needs
	who     authn.Authenticated
	ip      string
}

// ServeHTTP is the whole request flow: hostname → tenant, the path → a
// repository and a service, Basic auth for the service's scope, then the
// service itself. Everything is answered here; nothing runs a stack except
// the lanes a push dispatches [G1].
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
	// 2. The path: <prefix>/<repo>[.git]/{info/refs,git-upload-pack,git-receive-pack}.
	name, service, ok := c.route(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch service {
	case "info/refs":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	default:
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}
	req := request{tenant: tenant, name: name, service: service, action: "fetch", ip: c.clientIP(r)}
	if service == receivePackService || (service == "info/refs" && r.URL.Query().Get("service") == receivePackService) {
		req.action = "push"
	}
	// 3. Who: Basic auth for the scope the service needs.
	if !c.authenticate(w, r, &req) {
		return
	}
	// 4. The repository.
	repo, found, err := c.repos.Open(r.Context(), tenant, name)
	if err != nil {
		c.logger().Warn("git repo open failed", zap.String("tenant", tenant), zap.String("repo", name), zap.Error(err))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	// 5. The service.
	switch service {
	case "info/refs":
		c.serveInfoRefs(w, r, repo)
	case uploadPackService:
		c.serveUploadPack(w, r, repo)
	case receivePackService:
		c.serveReceivePack(w, r, repo, req)
	}
}

// route splits a path under the prefix into the repository name and the
// service. The dumb protocol's paths (HEAD, objects/…) are not served.
func (c *Controller) route(path string) (name, service string, ok bool) {
	if path != c.prefix && !strings.HasPrefix(path, c.prefix+"/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(path, c.prefix), "/")
	i := strings.IndexByte(rest, '/')
	if i <= 0 {
		return "", "", false
	}
	name, tail := rest[:i], rest[i+1:]
	name = strings.TrimSuffix(name, ".git")
	if !repoNameRE.MatchString(name) {
		return "", "", false
	}
	switch tail {
	case "info/refs", uploadPackService, receivePackService:
		return name, tail, true
	}
	return "", "", false
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

func (c *Controller) clientIP(r *http.Request) string {
	if c.clients == nil {
		h, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return h
	}
	return c.clients.IP(r)
}

func (c *Controller) secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// authenticate is Basic auth over TLS for the scope the request needs
// (`git:<repo>:fetch` or `git:<repo>:push`), through the login resolver
// the heads share. A request on a plaintext connection is refused before
// any credential is read, unless --git-insecure-auth (txco dev).
//
// Access is the credential's scopes and the tenant, nothing else: the head
// never compares the principal with the repository's owner, so a team
// member, another pony or a CI system holding a `git:<repo>:push`
// credential pushes the same way the owner does. Who is issued such a
// credential is the application's policy; ownership matters only to the
// administrative ops (removing the repository).
//
// The username is the account's login name (an address the chassis bound
// to a principal: `<slug>@<host>` → `pony:<slug>`); the password is a
// credential of that principal whose scopes cover the door. A wrong
// password, an unknown user, the right password for the wrong door and a
// disabled user all answer 401 the same way, so nothing is learned.
//
// A head started without a login resolver is CLOSED (503), never open: the
// prototype's open door is a test harness's choice (openWithoutAuth).
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request, req *request) bool {
	if !c.insecureAuth && !c.secure(r) {
		http.Error(w, "TLS required", http.StatusForbidden)
		return false
	}
	if c.auth == nil {
		if c.openWithoutAuth {
			return true
		}
		w.Header().Set("Retry-After", "60")
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return false
	}
	challenge := func() bool {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	username, pass, ok := r.BasicAuth()
	if !ok || username == "" {
		return challenge()
	}
	res := c.auth.Login(r.Context(), authn.Attempt{
		Tenant: req.tenant, Username: username, Password: pass, IP: req.ip,
		Want: authn.Scope{Domain: "git", Instance: req.name, Action: req.action},
	})
	switch res.Outcome {
	case authn.OutcomeOK:
	case authn.OutcomeThrottled:
		c.noteLogin("throttled", username, req.ip)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return false
	case authn.OutcomeError:
		c.logger().Warn("git login failed", zap.String("user", username), zap.Error(res.Err))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return false
	default:
		c.noteLogin(string(res.Outcome), username, req.ip)
		return challenge()
	}
	if c.pu.Admission != nil {
		if d := c.pu.Admission.Decide(req.tenant); !d.Admit {
			c.noteLogin("denied", username, req.ip)
			status := d.Status
			if status == 0 {
				status = http.StatusForbidden
			}
			if d.Retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.Retry.Seconds())))
			}
			http.Error(w, "service unavailable for this account", status)
			return false
		}
	}
	if !res.Cached {
		c.noteLogin("ok", username, req.ip, res.Who)
	}
	req.who = res.Who
	return true
}

// requestBody is one request's body, bounded, deadline-budgeted and
// gunzipped when the client compressed it (git compresses fetch requests
// over 1 KiB; it never compresses a push). The deadline covers the
// response too: a fetch streams a pack back on the same budget.
func (c *Controller) requestBody(w http.ResponseWriter, r *http.Request, max int64) (io.Reader, error) {
	budget := max
	if r.ContentLength > 0 {
		budget = r.ContentLength
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(c.now().Add(bodyBudget(budget)))
	_ = rc.SetWriteDeadline(c.now().Add(bodyBudget(budget) + 10*time.Minute))
	var body io.Reader = r.Body
	if max > 0 {
		body = http.MaxBytesReader(w, r.Body, max+1)
	}
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		body = gz
	}
	return body, nil
}

// noCache marks a protocol response: a cache in front would break the
// client's negotiation.
func noCache(h http.Header) {
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	h.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.Set("Pragma", "no-cache")
}
