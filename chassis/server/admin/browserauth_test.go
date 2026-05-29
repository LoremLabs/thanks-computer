package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// browserAuthTestServer wires the minimal subset of routes the
// browser-auth flow uses through a real httptest.Server, so cookie
// semantics (Set-Cookie → next request's Cookie header) round-trip
// the way they would in production. Tests that don't need the full
// HTTP layer (race semantics on ConsumeBootstrap, capability snapshot
// shape) can keep using direct handler calls + the in-package
// helpers below.
func browserAuthTestServer(t *testing.T, mode auth.AuthMode) (*Controller, *httptest.Server) {
	t.Helper()
	c := newTestController(t, config.Config{
		Personalities: "admin",
		AuthMode:      string(mode),
	})

	authCfg := auth.Config{
		Mode:           mode,
		Registry:       c.registry,
		Verifier:       c.verifier,
		Nonces:         c.nonces,
		Sessions:       c.registry,
		AllowedOrigins: []string{"http://localhost:6161", "http://127.0.0.1:6161"},
	}

	r := mux.NewRouter()
	r.HandleFunc("/auth/browser/exchange", c.handleBrowserExchange).Methods(http.MethodPost)
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return auth.Middleware(authCfg, next)
	})
	protected.HandleFunc("/auth/browser/session", c.handleBrowserSession).Methods(http.MethodGet)
	protected.HandleFunc("/auth/browser/session", c.handleBrowserSessionDelete).Methods(http.MethodDelete)
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/auth/browser/bootstrap", c.handleBrowserBootstrap).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/sessions", c.handleListBrowserSessions).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/sessions/{sessionID}", c.handleRevokeBrowserSession).Methods(http.MethodDelete)
	// Echo handler used to verify that a session cookie passes
	// through the middleware and stamps a browser-source auth context.
	protected.HandleFunc("/v1/tenants/{tenant}/whoami-browser", func(w http.ResponseWriter, r *http.Request) {
		ac := auth.FromContext(r.Context())
		if ac == nil {
			http.Error(w, "no ctx", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source":    ac.Source,
			"actor_id":  ac.ActorID,
			"tenant_id": ac.TenantID,
		})
	}).Methods(http.MethodPost, http.MethodGet)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return c, srv
}

// seedTenantActor inserts a tenant + actor + membership so the
// bootstrap handler's "this caller is a tenant member" path works.
func seedTenantActor(t *testing.T, c *Controller, actorID, tenantID, tenantSlug string, caps []string) {
	t.Helper()
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT OR IGNORE INTO tenants (tenant_id, slug, created_at) VALUES (?, ?, ?)`,
		tenantID, tenantSlug, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO actors (actor_id, label, kind, created_at) VALUES (?, ?, ?, ?)`,
		actorID, "test-actor", "cli", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed actor: %v", err)
	}
	capsJSON, _ := json.Marshal(caps)
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO actor_memberships (actor_id, tenant_id, capabilities_json, created_at) VALUES (?, ?, ?, ?)`,
		actorID, tenantID, string(capsJSON), "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// mintBootstrap drives the bootstrap handler via direct call with a
// synthetic auth.Context so tests don't have to construct signed
// requests. Returns the plaintext token.
func mintBootstrap(t *testing.T, c *Controller, actorID, tenantID string, caps []string) string {
	t.Helper()
	body, _ := json.Marshal(bootstrapRequest{Label: "test", TTLSeconds: 60})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/auth/browser/bootstrap", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"tenant": "default"})
	ctx := &auth.Context{
		Source:       "signed",
		ActorID:      actorID,
		TenantID:     tenantID,
		TenantSlug:   "default",
		Capabilities: caps,
	}
	r = r.WithContext(auth.WithContext(r.Context(), ctx))
	c.handleBrowserBootstrap(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}
	var resp bootstrapResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode bootstrap response: %v", err)
	}
	if !strings.HasPrefix(resp.Token, "btk_") {
		t.Errorf("token = %q, want btk_ prefix", resp.Token)
	}
	if resp.ExpiresInSeconds <= 0 {
		t.Errorf("expires_in_seconds = %d, want > 0", resp.ExpiresInSeconds)
	}
	return resp.Token
}

// TestBootstrapHappy verifies the mint endpoint returns the expected
// shape and stores a row in browser_bootstrap.
func TestBootstrapHappy(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})

	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})

	var count int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM browser_bootstrap WHERE actor_id = ?`,
		"actor_test").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("browser_bootstrap rows = %d, want 1", count)
	}
	_ = token
}

// TestBootstrapSuperAdminSnapshot — the most common first-boot case:
// a super_admin actor (created by `bootstrap-local` against a fresh
// chassis) carries no explicit capabilities in their signed-mode
// auth.Context. Their authority is the SuperAdmin flag, which the
// cookie path can't carry. The bootstrap handler must translate the
// flag into the admin:all wildcard so subsequent cookie-authed reads
// (RequireCapability("opstack:*:read") etc.) succeed instead of
// 403-ing the moment the browser fetches /stacks.
//
// Reproducer for the bug Matt hit: super_admin → login → 403 on the
// first dashboard fetch.
func TestBootstrapSuperAdminSnapshot(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO actors (actor_id, label, kind, super_admin, created_at)
		 VALUES (?, ?, ?, 1, ?)`,
		"actor_super", "test-super", "cli", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("seed actor: %v", err)
	}

	// Synthetic auth.Context for a super_admin: no Capabilities,
	// SuperAdmin=true — exactly what verifySigned + tenant middleware
	// produce for a first-boot actor.
	body, _ := json.Marshal(bootstrapRequest{Label: "test", TTLSeconds: 60})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/auth/browser/bootstrap", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"tenant": "default"})
	ctx := &auth.Context{
		Source:     "signed",
		ActorID:    "actor_super",
		TenantID:   "tnt_default",
		TenantSlug: "default",
		SuperAdmin: true,
		// Capabilities deliberately empty — matches reality.
	}
	r = r.WithContext(auth.WithContext(r.Context(), ctx))
	c.handleBrowserBootstrap(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
	}

	// The bootstrap row's capabilities_json should now read ["admin:all"].
	var capsJSON string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT capabilities_json FROM browser_bootstrap WHERE actor_id = ?`,
		"actor_super").Scan(&capsJSON); err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	if capsJSON != `["admin:all"]` {
		t.Errorf("super_admin snapshot = %q, want [\"admin:all\"]", capsJSON)
	}
}

// TestBootstrapRequiresTenant rejects a bootstrap call without a
// resolved tenant context.
func TestBootstrapRequiresTenant(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	body, _ := json.Marshal(bootstrapRequest{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/auth/browser/bootstrap", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"tenant": "default"})
	// No auth context.
	c.handleBrowserBootstrap(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 tenant_unresolved", w.Code)
	}
}

// TestExchangeHappy verifies the full round-trip through the HTTP layer:
// bootstrap → exchange returns a Set-Cookie → next request with that
// cookie reaches the protected echo handler as Source="browser".
func TestExchangeHappy(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeBoth)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})

	resp := postJSON(t, srv.URL+"/auth/browser/exchange",
		exchangeRequest{Token: token}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("exchange: %d body=%s", resp.StatusCode, string(body))
	}
	var sessionCookie *http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == auth.SessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("no %s cookie in response", auth.SessionCookieName)
	}
	if !sessionCookie.HttpOnly {
		t.Errorf("cookie is not HttpOnly")
	}
	if sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", sessionCookie.SameSite)
	}
	// The session cookie should now authenticate a follow-up request.
	echo := getWithCookie(t, srv.URL+"/v1/tenants/default/whoami-browser", sessionCookie)
	defer echo.Body.Close()
	if echo.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(echo.Body)
		t.Fatalf("whoami-browser: %d body=%s", echo.StatusCode, string(body))
	}
	var got map[string]any
	if err := json.NewDecoder(echo.Body).Decode(&got); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if got["source"] != "browser" {
		t.Errorf("source = %v, want browser", got["source"])
	}
	if got["actor_id"] != "actor_test" {
		t.Errorf("actor_id = %v, want actor_test", got["actor_id"])
	}
}

// TestExchangeInvalidToken: 404 + no session row.
func TestExchangeInvalidToken(t *testing.T) {
	_, srv := browserAuthTestServer(t, auth.ModeBoth)
	resp := postJSON(t, srv.URL+"/auth/browser/exchange",
		exchangeRequest{Token: "btk_not-a-real-token"}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got %d, want 404", resp.StatusCode)
	}
}

// TestExchangeRace: two parallel exchanges of the same token; only
// one wins. Verifies the conditional-UPDATE semantics from the
// registry.
func TestExchangeRace(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeBoth)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})

	var (
		wg        sync.WaitGroup
		successes int
		failures  int
		mu        sync.Mutex
	)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := postJSON(t, srv.URL+"/auth/browser/exchange",
				exchangeRequest{Token: token}, nil)
			defer resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if resp.StatusCode == http.StatusOK {
				successes++
			} else {
				failures++
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Errorf("got %d successful exchanges, want 1", successes)
	}
	if failures != 4 {
		t.Errorf("got %d failed exchanges, want 4", failures)
	}
}

// TestSessionGetWithCookie: cookie-authed → returns session info with
// source=browser.
func TestSessionGetWithCookie(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeBoth)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	resp := getWithCookie(t, srv.URL+"/auth/browser/session", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("session GET: %d body=%s", resp.StatusCode, string(body))
	}
	var info sessionInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.Source != "browser" {
		t.Errorf("source = %q, want browser", info.Source)
	}
	if info.ActorID != "actor_test" {
		t.Errorf("actor_id = %q, want actor_test", info.ActorID)
	}
	if info.OpenDev {
		t.Errorf("open_dev = true on a real session")
	}
}

// TestSessionGetOpenDev: in `both` mode with no creds, the chassis
// reports `{open_dev: true, source: "open"}` so the UI can skip the
// login flow.
func TestSessionGetOpenDev(t *testing.T) {
	_, srv := browserAuthTestServer(t, auth.ModeBoth)
	resp, err := http.Get(srv.URL + "/auth/browser/session")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %d, want 200 (open-dev passthrough); body=%s", resp.StatusCode, string(body))
	}
	var info sessionInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !info.OpenDev {
		t.Errorf("open_dev = false, want true in open-dev mode")
	}
	if info.Source != "open" {
		t.Errorf("source = %q, want open", info.Source)
	}
}

// TestSessionDelete: DELETE clears cookie + marks revoked + next call
// is 401-equivalent (session_invalid in signed mode).
func TestSessionDelete(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeSigned)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	// Delete the session.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/auth/browser/session", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE: got %d, want 200", resp.StatusCode)
	}

	// Reusing the cookie now must fail.
	resp2 := getWithCookie(t, srv.URL+"/v1/tenants/default/whoami-browser", cookie)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("reuse after revoke: got %d, want 401", resp2.StatusCode)
	}
}

// TestCSRFOriginMismatch: cookie-authed mutation with a bad Origin
// header → 401 (the middleware fails the request before the handler
// runs; CSRF and "session invalid" both map to invalid_signature).
func TestCSRFOriginMismatch(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeSigned)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/tenants/default/whoami-browser", strings.NewReader("{}"))
	req.AddCookie(cookie)
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("cross-origin POST: got %d, want 401", resp.StatusCode)
	}

	// Same request with the correct Origin should succeed.
	req2, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/v1/tenants/default/whoami-browser", strings.NewReader("{}"))
	req2.AddCookie(cookie)
	req2.Header.Set("Origin", "http://localhost:6161")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("POST allowed-origin: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("allowed-origin POST: got %d, want 200", resp2.StatusCode)
	}
}

// --admin-cors-origins entries land in the CSRF Origin allowlist
// (alongside the built-in dev defaults), with blank/whitespace
// entries skipped. Combined with TestCSRFOriginMismatch — which
// proves the middleware honors entries in this list — this covers
// the public-deploy "read-only → mutate-capable" switch end to end.
func TestAllowedBrowserOriginsIncludesConfigured(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:    "admin",
		AdminCorsOrigins: []string{"https://admin.example.com", "", "  "},
	})

	got := c.allowedBrowserOrigins()
	has := func(o string) bool {
		for _, v := range got {
			if v == o {
				return true
			}
		}
		return false
	}

	if !has("https://admin.example.com") {
		t.Errorf("configured origin missing from allowlist: %v", got)
	}
	// Built-in dev defaults preserved (zero-config txco dev still works).
	if !has("http://localhost:6161") || !has("http://127.0.0.1:6161") {
		t.Errorf("dev defaults dropped: %v", got)
	}
	// Blank / whitespace-only entries must never enter the allowlist
	// (an "" origin would otherwise match a missing Origin header).
	for _, v := range got {
		if strings.TrimSpace(v) == "" {
			t.Errorf("blank origin leaked into allowlist: %q in %v", v, got)
		}
	}
}

// TestListBrowserSessions returns the active sessions for the tenant.
func TestListBrowserSessions(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})

	// Two minted sessions for actor_test in tnt_default.
	for i := 0; i < 2; i++ {
		token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
		// Synthesize an exchange-equivalent call directly through the
		// registry so we don't need the HTTP stack here.
		b, err := c.registry.ConsumeBootstrap(context.Background(), token, "10.0.0.1")
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if _, err := c.registry.CreateSession(context.Background(), b, "ua/test", "10.0.0.1", time.Hour); err != nil {
			t.Fatalf("create_session: %v", err)
		}
	}

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/auth/sessions", nil), "tnt_default")
	c.handleListBrowserSessions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var resp listSessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sessions) != 2 {
		t.Errorf("got %d sessions, want 2", len(resp.Sessions))
	}
}

// TestAdminRevokeSession: admin DELETE on a session_id in this tenant
// works; admin in another tenant gets 404 (so existence doesn't leak).
func TestAdminRevokeSession(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	b, _ := c.registry.ConsumeBootstrap(context.Background(), token, "10.0.0.1")
	sess, _ := c.registry.CreateSession(context.Background(), b, "ua/test", "10.0.0.1", time.Hour)

	// Same-tenant admin revoke succeeds.
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete,
		"/v1/tenants/default/auth/sessions/"+sess.SessionID, nil), "tnt_default")
	r = mux.SetURLVars(r, map[string]string{
		"tenant":    "default",
		"sessionID": sess.SessionID,
	})
	c.handleRevokeBrowserSession(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("same-tenant revoke: %d %s", w.Code, w.Body.String())
	}

	// Re-revoke is a no-op (200 because RevokeSession is idempotent).
	w2 := httptest.NewRecorder()
	r2 := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete,
		"/v1/tenants/default/auth/sessions/"+sess.SessionID, nil), "tnt_default")
	r2 = mux.SetURLVars(r2, map[string]string{
		"tenant":    "default",
		"sessionID": sess.SessionID,
	})
	c.handleRevokeBrowserSession(w2, r2)
	if w2.Code != http.StatusOK {
		t.Errorf("idempotent revoke: %d", w2.Code)
	}

	// Different-tenant revoke gets 404 (doesn't leak existence).
	w3 := httptest.NewRecorder()
	r3 := withTenantAdminCtx(httptest.NewRequest(http.MethodDelete,
		"/v1/tenants/other/auth/sessions/"+sess.SessionID, nil), "tnt_other")
	r3 = mux.SetURLVars(r3, map[string]string{
		"tenant":    "other",
		"sessionID": sess.SessionID,
	})
	c.handleRevokeBrowserSession(w3, r3)
	if w3.Code != http.StatusNotFound {
		t.Errorf("cross-tenant revoke: %d, want 404 (no existence leak)", w3.Code)
	}
}

// TestClampBootstrapTTL exercises the TTL clamp directly so the
// boundary cases aren't only implicit in handler tests.
func TestClampBootstrapTTL(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, bootstrapDefaultTTL},
		{-1, bootstrapDefaultTTL},
		{1, bootstrapMinTTL},
		{30, 30 * time.Second},
		{900, bootstrapMaxTTL},
	}
	for _, tc := range cases {
		if got := clampBootstrapTTL(tc.in); got != tc.want {
			t.Errorf("clamp(%d) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// secretBrowserServer wires the browser-auth middleware in front of
// the read-only secret endpoint through a real httptest.Server, so a
// session cookie minted by the exchange flow round-trips into the
// secret route the way it does in production. This is the load-bearing
// prerequisite for the admin-UI secrets view, which calls
// GET /v1/tenants/{slug}/secrets with credentials: 'same-origin'.
func secretBrowserServer(t *testing.T, c *Controller) *httptest.Server {
	t.Helper()
	authCfg := auth.Config{
		Mode:           auth.ModeSigned,
		Registry:       c.registry,
		Verifier:       c.verifier,
		Nonces:         c.nonces,
		Sessions:       c.registry,
		AllowedOrigins: []string{"http://localhost:6161", "http://127.0.0.1:6161"},
	}
	r := mux.NewRouter()
	r.HandleFunc("/auth/browser/exchange", c.handleBrowserExchange).Methods(http.MethodPost)
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return auth.Middleware(authCfg, next)
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/secrets", c.handleListSecrets).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// TestSecretEndpointAcceptsSessionCookie: a session cookie carrying
// secret:*:read authenticates GET /secrets through the same middleware
// the CLI uses, and the in-handler capability gate sees the cookie's
// capabilities. Pins that the UI's cookie auth reaches the secret
// routes (they postdate the browser-auth tests).
func TestSecretEndpointAcceptsSessionCookie(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities: "admin",
		AuthMode:      string(auth.ModeSigned),
	})
	wireSecrets(t, c)
	// opstack:*:read is the baseline "can mint a browser session" gate
	// on bootstrap; secret:*:read is what the session must carry to
	// pass the secret endpoint's own gate.
	caps := []string{"opstack:*:read", "secret:*:read"}
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", caps)
	srv := secretBrowserServer(t, c)

	token := mintBootstrap(t, c, "actor_test", "tnt_default", caps)
	cookie := exchangeForCookie(t, srv.URL, token)

	resp := getWithCookie(t, srv.URL+"/v1/tenants/default/secrets", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /secrets with session cookie: %d body=%s", resp.StatusCode, string(body))
	}
	var listResp listSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Empty store → empty (non-nil) list. The point is the cookie
	// authenticated and the secret:*:read gate passed.
	if listResp.Secrets == nil {
		t.Errorf("secrets list is nil; want non-nil (possibly empty)")
	}
}

// TestSecretEndpointDeniesCookieWithoutCapability: a valid session
// cookie that lacks secret:*:read is rejected with 403 at the
// capability gate (not 401 — it authenticates fine, it just isn't
// authorized). This is what the UI's greyed-out write controls and the
// read-vs-write capability split rely on.
func TestSecretEndpointDeniesCookieWithoutCapability(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities: "admin",
		AuthMode:      string(auth.ModeSigned),
	})
	wireSecrets(t, c)
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	srv := secretBrowserServer(t, c)

	// Cookie carries only opstack:*:read — no secret capability.
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	resp := getWithCookie(t, srv.URL+"/v1/tenants/default/secrets", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /secrets without secret cap: %d body=%s, want 403",
			resp.StatusCode, string(body))
	}
}

// --- helpers ----------------------------------------------------------

func postJSON(t *testing.T, url string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func getWithCookie(t *testing.T, url string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// exchangeForCookie POSTs to /auth/browser/exchange, returns the
// session cookie. Fails the test if exchange isn't successful.
func exchangeForCookie(t *testing.T, baseURL, token string) *http.Cookie {
	t.Helper()
	resp := postJSON(t, baseURL+"/auth/browser/exchange",
		exchangeRequest{Token: token}, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("exchange for cookie: %d body=%s", resp.StatusCode, string(body))
	}
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("no session cookie in exchange response")
	return nil
}
