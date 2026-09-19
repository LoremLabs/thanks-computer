package calendar

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/emersion/go-webdav/caldav"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// principal is the authenticated account on a request's context.
type principal struct {
	tenant   string
	username string
	acct     chcal.Account
	clientIP string
	// who is the principal the credential signed in as, pinned on every
	// envelope this request dispatches.
	who authn.Authenticated
}

type ctxKeyPrincipal struct{}

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(ctxKeyPrincipal{}).(principal)
	return p, ok
}

// pathParts splits a path under the prefix into its segments (decoded by
// net/http already): [username], [username calendars], [username
// calendars <name>], [username calendars <name> <resource>].
func (c *Controller) pathParts(p string) []string {
	p = strings.TrimPrefix(p, c.prefix)
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ServeHTTP is the whole request flow. Every branch that reads or writes
// the store is answered here or by the CalDAV library over backend.go;
// none of them touches the bus except through the lanes.
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
	// 2. Discovery: RFC 6764. Unauthenticated; the redirect target is the
	// root, whose PROPFIND (authenticated) names the principal.
	if r.URL.Path == "/.well-known/caldav" {
		// An absolute Location: every client resolves it the same way.
		scheme := "http"
		if c.secure(r) {
			scheme = "https"
		}
		http.Redirect(w, r, scheme+"://"+r.Host+c.prefix+"/", http.StatusMovedPermanently)
		return
	}
	parts := c.pathParts(r.URL.Path)
	// 3. The ICS feed: an opaque bearer URL, no auth.
	if len(parts) == 2 && parts[0] == "feed" {
		c.serveFeed(w, r, tenant, strings.TrimSuffix(parts[1], ".ics"))
		return
	}
	// 4. Everything else is Basic-authenticated over TLS.
	pr, ok := c.authenticate(w, r, tenant)
	if !ok {
		return
	}
	// 5. No enumeration: the first segment, when present, is the caller.
	if len(parts) > 0 && parts[0] != pr.username {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := context.WithValue(r.Context(), ctxKeyPrincipal{}, pr)
	r = r.WithContext(ctx)
	// 6. What the library cannot do at v0.7.0: MKCALENDAR, PROPPATCH, a
	// DELETE of a calendar, the object size cap.
	switch {
	case r.Method == "PROPFIND" && len(parts) <= 1:
		// Root + principal discovery, answered here (see discovery.go).
		c.serveDiscoveryPropfind(w, r, pr, parts)
		return
	case r.Method == "MKCALENDAR" || (r.Method == "MKCOL" && len(parts) == 3 && parts[1] == "calendars"):
		c.serveMkcalendar(w, r, pr, parts)
		return
	case r.Method == "PROPPATCH":
		c.serveProppatch(w, r, pr, parts)
		return
	case r.Method == http.MethodDelete && len(parts) == 3 && parts[1] == "calendars":
		c.serveRemoveCalendar(w, r, pr, parts)
		return
	case r.Method == http.MethodPut:
		if r.ContentLength > c.maxBytes {
			http.Error(w, fmt.Sprintf("object over %d bytes", c.maxBytes), http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, c.maxBytes)
	}
	h := caldav.Handler{Backend: &backend{c: c}, Prefix: c.prefix}
	h.ServeHTTP(w, r)
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

// authenticate is Basic auth: TLS first (before a credential is read), the
// account the username names, then the password through the login resolver
// the heads share (chassis/authn: the username's binding finds the
// principal, the credential id in the password finds the credential, one
// argon2id verify, and the credential's scopes must cover
// `calendar:<username>:login` — with one login cache and one set of
// throttles), then the account's status, admission, and the tenant match.
func (c *Controller) authenticate(w http.ResponseWriter, r *http.Request, tenant string) (principal, bool) {
	ip := clientIP(r)
	if !c.secure(r) && !c.insecureAuth {
		http.Error(w, "TLS required", http.StatusForbidden)
		return principal{}, false
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" {
		w.Header().Set("WWW-Authenticate", `Basic realm="calendar", charset="UTF-8"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return principal{}, false
	}
	// A bare local part completes to the request's host: an account dialog
	// shows the address's local part as the "user name" and a person types
	// just that (prod, 2026-09-06: `front-desk` against the pony's own
	// hostname), and the head serves the address's own domain. A completed
	// name that names no account fails exactly like any other.
	if !strings.Contains(user, "@") {
		user = user + "@" + hostOnly(r.Host)
	}
	username := chcal.NormalizeUsername(user)
	deny := func(outcome string) (principal, bool) {
		c.noteLogin(outcome, username, ip)
		w.Header().Set("WWW-Authenticate", `Basic realm="calendar", charset="UTF-8"`)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return principal{}, false
	}
	tooMany := func() (principal, bool) {
		c.noteLogin("throttled", username, ip)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many authentication attempts", http.StatusTooManyRequests)
		return principal{}, false
	}
	acct, found, err := c.store.GetAccount(r.Context(), username)
	if err != nil {
		c.pu.Logger.Warn("calendar login lookup failed", zap.String("user", username), zap.String("err", err.Error()))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return principal{}, false
	}
	if !found {
		if c.auth.Miss(ip, username, pass) == authn.OutcomeThrottled {
			return tooMany()
		}
		return deny("failed")
	}
	res := c.auth.Login(r.Context(), authn.Attempt{
		Tenant: acct.Tenant, Username: acct.Username, Password: pass, IP: ip,
		Want: authn.Scope{Domain: "calendar", Instance: acct.Username, Action: "login"},
	})
	switch res.Outcome {
	case authn.OutcomeOK:
	case authn.OutcomeThrottled:
		return tooMany()
	case authn.OutcomeError:
		c.pu.Logger.Warn("calendar login failed", zap.String("user", username), zap.Error(res.Err))
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return principal{}, false
	default:
		return deny(string(res.Outcome))
	}
	cached := res.Cached
	if acct.Status != chcal.StatusActive {
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
	if cached {
		// Every request carries Basic auth and a calendar app polls its collections, so a
		// hit is the same client's next request, not a new login. It is
		// counted; the line is logged once per password check, i.e. once
		// per cache TTL per node. Refusals above log every time.
		c.countLogin("ok")
	} else {
		c.noteLogin("ok", username, ip, res.Who)
	}
	return principal{tenant: tenant, username: acct.Username, acct: acct, clientIP: ip, who: res.Who}, true
}

// serveFeed answers GET/HEAD <prefix>/feed/<token>.ics from the store.
func (c *Controller) serveFeed(w http.ResponseWriter, r *http.Request, tenant, token string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if token == "" || len(token) > 128 {
		http.NotFound(w, r)
		return
	}
	cal, ok, err := c.store.CalendarByFeedHash(r.Context(), feedTokenHash(token))
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !ok || cal.Tenant != tenant {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf(`"%s-%d"`, cal.ID, cal.SyncToken)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", fmt.Sprintf("private, max-age=%d", c.feedMaxAge))
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	objs, err := c.store.ListObjects(r.Context(), cal.ID, chcal.ListOpts{})
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	name := cal.DisplayName
	if name == "" {
		name = cal.Name
	}
	body, err := chcal.FeedBytes(name, cal.Description, objs)
	if err != nil {
		http.Error(w, "feed render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// serveRemoveCalendar is DELETE on a calendar collection.
func (c *Controller) serveRemoveCalendar(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	cal, found, err := c.store.GetCalendar(r.Context(), pr.tenant, pr.username, parts[2])
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	m := mutation{tenant: pr.tenant, account: pr.username, who: pr.who, op: opRemove, calendar: refOf(cal), clientIP: pr.clientIP}
	if status, msg := c.gate(&cal, &pr.acct, chcal.VerbRemove, &m); status != 0 {
		http.Error(w, msg, status)
		return
	}
	if _, err := c.store.RemoveCalendar(r.Context(), cal.ID); err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	c.after(&cal, &pr.acct, chcal.VerbRemove, m)
	w.WriteHeader(http.StatusNoContent)
}

// gate applies the calendar's policy for one verb before a commit:
// deny ⇒ 403; stack ⇒ ask the lanes (a refusal is the status its code
// maps to); local/observe ⇒ proceed. A stack's rewrite lands on m.
func (c *Controller) gate(cal *chcal.Calendar, acct *chcal.Account, verb string, m *mutation) (status int, msg string) {
	switch chcal.PolicyMode(cal, acct, verb) {
	case chcal.ModeDeny:
		return http.StatusForbidden, "this calendar is read-only"
	case chcal.ModeStack:
		a := c.lanes.ask(*m)
		if !a.ok {
			return answerStatus(a.code), a.msg
		}
		m.rewrite = a.rewrite
	}
	return 0, ""
}

// after is the post-commit half: observe ⇒ the lanes, fire-and-forget.
func (c *Controller) after(cal *chcal.Calendar, acct *chcal.Account, verb string, m mutation) {
	if chcal.PolicyMode(cal, acct, verb) == chcal.ModeObserve {
		c.lanes.observe(m)
	}
}

func answerStatus(code string) int {
	switch code {
	case "limit":
		return http.StatusInsufficientStorage
	case "unavailable":
		return http.StatusServiceUnavailable
	}
	return http.StatusForbidden
}

var errForbidden = errors.New("forbidden")
