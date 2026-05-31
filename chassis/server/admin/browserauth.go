package admin

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// Browser-friendly auth endpoints (Phase 2b).
//
// Three flat endpoints (`/auth/browser/{exchange,session}`) plus one
// tenant-scoped endpoint (`/v1/tenants/{t}/auth/browser/bootstrap`).
// The split is intentional: bootstrap binds a session to a specific
// tenant up front, so it lives in the tenant subrouter (URL is the
// natural place for that scoping); exchange and session-info operate
// on the resulting cookie which already carries the tenant.
//
// Design doc: internal docs/todo-admin-ui-browser-auth.md.

const (
	// bootstrapTokenTTL is the default time-window between mint and
	// consume. Short by design — the token is a relay, not a session.
	bootstrapDefaultTTL = 60 * time.Second
	bootstrapMinTTL     = 15 * time.Second
	bootstrapMaxTTL     = 300 * time.Second

	// sessionTTL is the absolute ceiling on a session's lifetime.
	// Renewals (last_seen_at) don't extend past this.
	sessionDefaultTTL = 7 * 24 * time.Hour
)

// --- wire types -------------------------------------------------------

type bootstrapRequest struct {
	Label      string `json:"label,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type bootstrapResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
	URL              string `json:"url"`
}

type exchangeRequest struct {
	Token string `json:"token"`
}

type exchangeResponse struct {
	SessionID    string   `json:"session_id"`
	ActorID      string   `json:"actor_id"`
	TenantID     string   `json:"tenant_id"`
	Capabilities []string `json:"capabilities"`
	ExpiresAt    string   `json:"expires_at"`
}

type sessionInfoResponse struct {
	SessionID    string   `json:"session_id,omitempty"`
	ActorID      string   `json:"actor_id,omitempty"`
	TenantID     string   `json:"tenant_id,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	Source       string   `json:"source"`
	OpenDev      bool     `json:"open_dev,omitempty"`
}

type sessionRecord struct {
	SessionID  string  `json:"session_id"`
	ActorID    string  `json:"actor_id"`
	TenantID   string  `json:"tenant_id"`
	UA         string  `json:"ua,omitempty"`
	IP         string  `json:"ip,omitempty"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at"`
	LastSeenAt string  `json:"last_seen_at"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	RevokedBy  string  `json:"revoked_by,omitempty"`
}

type listSessionsResponse struct {
	Sessions []sessionRecord `json:"sessions"`
}

// --- bootstrap (tenant-scoped, signed) --------------------------------

// handleBrowserBootstrap mints a single-use token the caller hands to
// a browser. Requires a signed (or session) auth context with a
// resolved tenant. Capabilities are snapshotted from auth.Context so
// the eventual session matches what the actor is allowed to do
// *right now*.
func (c *Controller) handleBrowserBootstrap(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.ActorID == "" || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	// Browser bootstrap is a self-service action — the actor mints a
	// token for themselves. The capability check here is the same one
	// any other tenant-scoped read goes through, so non-members get
	// rejected at the membership-resolve layer before this runs.
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	var req bootstrapRequest
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
			return
		}
	}
	ttl := clampBootstrapTTL(req.TTLSeconds)

	// Capability snapshot for the eventual session. For non-super-admin
	// signed callers, `ac.Capabilities` is the resolved tenant
	// membership (populated by resolveTenantMiddleware) — exactly what
	// we want to mirror into the session.
	//
	// For super-admin callers, `ac.Capabilities` is empty: their
	// authority comes from the `super_admin` flag, which
	// resolveTenantMiddleware deliberately leaves alone so chassis-wide
	// emergency access stays open. Cookie-authed sessions don't carry
	// that flag (verifyCookie reads the snapshotted capabilities slice
	// only), so we'd end up with a logged-in super_admin who gets 403
	// on every read. Translate the flag into the equivalent admin:all
	// wildcard at snapshot time — same shape basic-auth and open-dev
	// already use, and RequireCapability's admin:all → *:*:*
	// expansion catches every check downstream.
	caps := ac.Capabilities
	if ac.SuperAdmin {
		caps = []string{"admin:all"}
	}

	plaintext, _, err := c.registry.CreateBootstrap(
		r.Context(), ac.ActorID, ac.TenantID, caps, ac.SuperAdmin, req.Label, ttl)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create_bootstrap", map[string]any{"err": err.Error()})
		return
	}

	c.pu.Logger.Info("browser bootstrap minted",
		zap.String("actor", ac.ActorID),
		zap.String("tenant", ac.TenantID),
		zap.String("label", req.Label),
		zap.Bool("super_admin", ac.SuperAdmin),
		zap.Duration("ttl", ttl))

	writeJSON(w, http.StatusOK, bootstrapResponse{
		Token:            plaintext,
		ExpiresInSeconds: int(ttl.Seconds()),
		URL:              browserLoginURL(r, plaintext),
	})
}

func clampBootstrapTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return bootstrapDefaultTTL
	}
	t := time.Duration(seconds) * time.Second
	if t < bootstrapMinTTL {
		return bootstrapMinTTL
	}
	if t > bootstrapMaxTTL {
		return bootstrapMaxTTL
	}
	return t
}

// browserLoginURL builds the user-facing URL the CLI prints (and
// `open`s) for the operator. The browser lands at `/admin/#login?t=…`
// where the Svelte UI extracts the token and POSTs it to /exchange.
// Hash-routing (#) keeps the token out of HTTP server logs.
func browserLoginURL(r *http.Request, token string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	return scheme + "://" + host + "/admin/#login?t=" + token
}

// --- exchange (flat, unsigned, throttled) -----------------------------

// handleBrowserExchange consumes a bootstrap token and sets a session
// cookie. Lives outside the protected subrouter — the only proof of
// identity is the token itself.
func (c *Controller) handleBrowserExchange(w http.ResponseWriter, r *http.Request) {
	var req exchangeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", nil)
		return
	}

	consumerIP := clientIP(r)
	b, err := c.registry.ConsumeBootstrap(r.Context(), req.Token, consumerIP)
	if err != nil {
		if errors.Is(err, registry.ErrBootstrapInvalid) {
			writeJSONError(w, http.StatusNotFound, "bootstrap_invalid", nil)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "consume_bootstrap", map[string]any{"err": err.Error()})
		return
	}

	sess, err := c.registry.CreateSession(r.Context(), b,
		strings.TrimSpace(r.Header.Get("User-Agent")), consumerIP, sessionDefaultTTL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create_session", map[string]any{"err": err.Error()})
		return
	}

	http.SetCookie(w, c.sessionCookie(r, sess.SessionID, sessionDefaultTTL))

	c.pu.Logger.Info("browser session minted",
		zap.String("session", sess.SessionID),
		zap.String("actor", sess.ActorID),
		zap.String("tenant", sess.TenantID),
		zap.String("ip", consumerIP))

	writeJSON(w, http.StatusOK, exchangeResponse{
		SessionID:    sess.SessionID,
		ActorID:      sess.ActorID,
		TenantID:     sess.TenantID,
		Capabilities: sess.Capabilities,
		ExpiresAt:    sess.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// sessionCookie builds the Set-Cookie header value for a fresh session.
// Mirrors the design doc: HttpOnly + SameSite=Strict; Secure unless
// the request arrived over plain HTTP localhost (the dev-server
// carve-out).
func (c *Controller) sessionCookie(r *http.Request, sessionID string, ttl time.Duration) *http.Cookie {
	cookie := &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	}
	if isSecureRequest(r) {
		cookie.Secure = true
	}
	return cookie
}

// expiredCookie produces a Set-Cookie value that clears the session
// cookie on the browser. Sent on logout.
func (c *Controller) expiredCookie(r *http.Request) *http.Cookie {
	cookie := &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
	if isSecureRequest(r) {
		cookie.Secure = true
	}
	return cookie
}

// isSecureRequest decides whether the Secure flag should be set on the
// session cookie. HTTPS — yes. HTTP via a TLS-terminating proxy that
// stamped X-Forwarded-Proto=https — yes. Plain HTTP to localhost or
// 127.0.0.1 — no (browsers reject Secure cookies on plain http even
// when the origin is localhost; the Phase 2b doc carves this out for
// `txco dev`-style local development).
func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	// Default deny — a request over HTTP to a non-localhost host
	// shouldn't get a usable cookie either way; sending Secure=false
	// lets the browser store it (good for `txco dev` over a LAN
	// hostname) without exposing it on a real production HTTPS chassis
	// (where r.TLS would be set).
	return false
}

// --- session info / delete (flat, cookie or signed) ------------------

// handleBrowserSession returns the current session's info. Open-dev
// mode (chassis is `--auth-mode=both` with no admin user configured)
// returns `{open_dev: true, source: "open"}` instead of a 401 so the
// UI can detect "you're already authed by virtue of the chassis being
// in open dev" and skip the login flow.
func (c *Controller) handleBrowserSession(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil {
		// Open-dev passthrough wouldn't reach here with ac=nil because
		// the middleware stamps openDevContext. So nil = no auth at all,
		// which we surface as 401.
		writeJSONError(w, http.StatusUnauthorized, "no_session", nil)
		return
	}
	if ac.Source == "open" {
		writeJSON(w, http.StatusOK, sessionInfoResponse{
			Source:  "open",
			OpenDev: true,
		})
		return
	}
	resp := sessionInfoResponse{
		Source:       ac.Source,
		ActorID:      ac.ActorID,
		TenantID:     ac.TenantID,
		Capabilities: ac.Capabilities,
	}
	if ac.Source == "browser" {
		if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
			resp.SessionID = cookie.Value
			if sess, err := c.registry.GetSession(r.Context(), cookie.Value); err == nil && sess != nil {
				resp.ExpiresAt = sess.ExpiresAt.UTC().Format(time.RFC3339)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleBrowserSessionDelete revokes the current session ("sign out")
// and clears the cookie. Idempotent — revoke-already-revoked is a
// no-op at the registry layer.
func (c *Controller) handleBrowserSessionDelete(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil {
		writeJSONError(w, http.StatusUnauthorized, "no_session", nil)
		return
	}
	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		// No cookie to revoke; still clear it on the response in case
		// the browser is holding a stale one.
		http.SetCookie(w, c.expiredCookie(r))
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
		return
	}
	by := ac.ActorID
	if by == "" {
		by = ac.Source
	}
	if err := c.registry.RevokeSession(r.Context(), cookie.Value, by); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revoke_session", map[string]any{"err": err.Error()})
		return
	}
	http.SetCookie(w, c.expiredCookie(r))
	c.pu.Logger.Info("browser session revoked",
		zap.String("session", cookie.Value),
		zap.String("by", by))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// --- admin sessions (tenant-scoped, list + revoke) -------------------

// handleListBrowserSessions returns every session in the tenant —
// active and historic — newest first. Gated on opstack:*:read so any
// tenant member can audit; admin gating can be tightened later if
// needed.
func (c *Controller) handleListBrowserSessions(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	sessions, err := c.registry.ListSessions(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_sessions", map[string]any{"err": err.Error()})
		return
	}
	out := listSessionsResponse{Sessions: make([]sessionRecord, 0, len(sessions))}
	for _, s := range sessions {
		rec := sessionRecord{
			SessionID:  s.SessionID,
			ActorID:    s.ActorID,
			TenantID:   s.TenantID,
			UA:         s.UA,
			IP:         s.IP,
			CreatedAt:  s.CreatedAt.UTC().Format(time.RFC3339),
			ExpiresAt:  s.ExpiresAt.UTC().Format(time.RFC3339),
			LastSeenAt: s.LastSeenAt.UTC().Format(time.RFC3339),
			RevokedBy:  s.RevokedBy,
		}
		if s.RevokedAt != nil {
			ts := s.RevokedAt.UTC().Format(time.RFC3339)
			rec.RevokedAt = &ts
		}
		out.Sessions = append(out.Sessions, rec)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeBrowserSession revokes a specific session by id. Gated
// on opstack:*:update — anyone who can mutate the opstack can also
// kick a session.
func (c *Controller) handleRevokeBrowserSession(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	sessionID := mux.Vars(r)["sessionID"]
	if sessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_session_id", nil)
		return
	}
	// Verify the session is in this tenant before revoking, so an
	// admin in tenant A can't drop sessions belonging to tenant B by
	// guessing IDs.
	sess, err := c.registry.GetSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "session_not_found", nil)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup_session", map[string]any{"err": err.Error()})
		return
	}
	if sess.TenantID != ac.TenantID {
		// Don't leak existence across tenants — 404 not 403.
		writeJSONError(w, http.StatusNotFound, "session_not_found", nil)
		return
	}
	by := ac.ActorID
	if by == "" {
		by = ac.Source
	}
	if err := c.registry.RevokeSession(r.Context(), sessionID, by); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revoke_session", map[string]any{"err": err.Error()})
		return
	}
	c.pu.Logger.Info("browser session revoked by admin",
		zap.String("session", sessionID),
		zap.String("by", by),
		zap.String("tenant", ac.TenantID))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// allowedBrowserOrigins returns the Origin allowlist for the CSRF
// check on cookie-authed mutations. The list is:
//
//   - The chassis's own admin URL (where the SPA is served).
//   - The Vite dev server (http://localhost:6161 + http://127.0.0.1:6161),
//     so `npm run dev` against a local chassis works without operator
//     configuration.
//   - Anything in `--admin-cors-origins` (comma-separated), for
//     deployments that serve the UI from a different host.
//
// An empty result means "no origin check" (the middleware then accepts
// any Origin or no Origin). We don't currently default to empty —
// the dev defaults above are enough to keep `txco dev` working
// without footguns.
func (c *Controller) allowedBrowserOrigins() []string {
	out := []string{
		"http://localhost:6161",
		"http://127.0.0.1:6161",
	}
	// Chassis's own admin address — derive from the bind address so a
	// chassis on a non-default port still works.
	addr := c.pu.Conf.AdminAddr
	if addr != "" {
		// AdminAddr is `host:port`; for `:8081` that's empty host
		// which we treat as localhost (the loopback fallback the
		// docs steer dev users toward).
		host := addr
		if strings.HasPrefix(addr, ":") {
			host = "localhost" + addr
		}
		out = append(out,
			"http://"+host,
			"http://127.0.0.1"+adminPortSuffix(addr),
		)
	}
	// Operator-supplied origins (--admin-cors-origins) — the public
	// hostname(s) the admin UI is served from. Without this every
	// cookie-authed mutation from that origin is rejected as a CSRF
	// mismatch, so the UI is read-only at a public deploy. Blank
	// entries (trailing comma / stray whitespace) are skipped.
	for _, o := range c.pu.Conf.AdminCorsOrigins {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// adminPortSuffix returns the `:port` portion of a host:port (or
// `:port`) address, or "" if there's none.
func adminPortSuffix(addr string) string {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return ""
	}
	return addr[i:]
}

// (clientIP is defined in throttle.go; reused here.)
