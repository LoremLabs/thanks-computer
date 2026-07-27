package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The re-auth marker closes the "sign out didn't stick" hole: signing out
// of the admin UI revokes the browser session, but the CLI still holds the
// long-lived ed25519 key that minted it, so `txco ui cloud` would sign a
// fresh bootstrap and walk straight back in without ever visiting the
// identity provider. Sign-out now stamps the actor, bootstrap refuses while
// the stamp is set, and an OIDC enrollment clears it.

// bootstrapStatus drives the bootstrap handler directly (like mintBootstrap)
// but returns the status + decoded error body instead of failing the test on
// a non-200 — these tests are specifically about the refusal.
func bootstrapStatus(t *testing.T, c *Controller, actorID, tenantID string, caps []string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(bootstrapRequest{Label: "test", TTLSeconds: 60})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/auth/browser/bootstrap", bytes.NewReader(body))
	r = mux.SetURLVars(r, map[string]string{"tenant": "default"})
	r = r.WithContext(auth.WithContext(r.Context(), &auth.Context{
		Source:       "signed",
		ActorID:      actorID,
		TenantID:     tenantID,
		TenantSlug:   "default",
		Capabilities: caps,
	}))
	c.handleBrowserBootstrap(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// signOutViaServer performs the real HTTP DELETE so the cookie round-trips
// through the middleware exactly as a browser's would.
func signOutViaServer(t *testing.T, srvURL string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, srvURL+"/auth/browser/session", nil)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func reauthStamp(t *testing.T, c *Controller, actorID string) string {
	t.Helper()
	var at *string
	if err := c.pu.AuthDB.QueryRowContext(context.Background(),
		`SELECT reauth_required_at FROM actors WHERE actor_id = ?`, actorID).Scan(&at); err != nil {
		t.Fatalf("read reauth_required_at: %v", err)
	}
	if at == nil {
		return ""
	}
	return *at
}

// TestSignOutMarksReauthRequired is the core regression for the reported bug:
// after signing out in the admin UI, holding the signing key must no longer
// be enough to mint another browser session.
func TestSignOutMarksReauthRequired(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeSigned)
	c.oauthIssuer = testOAuthIssuer // chassis can offer a re-auth path
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	if got := reauthStamp(t, c, "actor_test"); got != "" {
		t.Fatalf("actor marked before sign-out: %q", got)
	}

	resp := signOutViaServer(t, srv.URL, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d, want 200", resp.StatusCode)
	}
	if got := reauthStamp(t, c, "actor_test"); got == "" {
		t.Error("sign-out did not stamp reauth_required_at")
	}

	// The actual user-visible symptom: `txco ui cloud` must now be refused.
	code, body := bootstrapStatus(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	if code != http.StatusForbidden {
		t.Fatalf("bootstrap after sign-out: %d, want 403", code)
	}
	if body["error"] != "reauth_required" {
		t.Errorf("error = %v, want reauth_required", body["error"])
	}
}

// TestSignOutNoIssuerDoesNotMark guards the lockout case. A self-hosted
// chassis with no OAuth issuer has no way to CLEAR the marker, so stamping
// it would leave the operator permanently unable to open their own admin UI.
func TestSignOutNoIssuerDoesNotMark(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeSigned)
	// c.oauthIssuer deliberately left empty — open-core default.
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_test", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	if resp := signOutViaServer(t, srv.URL, cookie); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d, want 200", resp.StatusCode)
	}
	if got := reauthStamp(t, c, "actor_test"); got != "" {
		t.Errorf("reauth_required_at = %q on a chassis with no OAuth issuer; "+
			"this would lock the operator out of the admin UI", got)
	}
	// And the CLI can still sign back in, exactly as before this change.
	if code, body := bootstrapStatus(t, c, "actor_test", "tnt_default",
		[]string{"opstack:*:read"}); code != http.StatusOK {
		t.Errorf("bootstrap = %d (%v), want 200 on a no-issuer chassis", code, body)
	}
}

// TestSignOutOpenDevDoesNotMark: an open-dev context carries no ActorID, so
// there is no actor row to stamp. Asserts we don't blow up or stamp something
// arbitrary on that path.
func TestSignOutOpenDevDoesNotMark(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeBoth)
	c.oauthIssuer = testOAuthIssuer
	seedTenantActor(t, c, "actor_test", "tnt_default", "default", []string{"opstack:*:read"})

	// No cookie: the middleware resolves an open-dev context (ActorID "").
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/auth/browser/session", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d, want 200", resp.StatusCode)
	}
	if got := reauthStamp(t, c, "actor_test"); got != "" {
		t.Errorf("open-dev sign-out stamped an unrelated actor: %q", got)
	}
}

// TestOAuthEnrollClearsReauthMarker closes the loop: going through the
// identity provider again is what lets the machine back in.
func TestOAuthEnrollClearsReauthMarker(t *testing.T) {
	env := newOAuthTestEnv(t)
	c := env.c

	// First enroll creates the tenant + actor for this subject/key.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	code, body := env.enroll(t, oauthEnrollRequest{
		IDToken:    env.token(t, "user-1"),
		PublicKey:  pubB64,
		Label:      "matt@macbook",
		TenantSlug: "mankins",
	})
	if code != http.StatusOK {
		t.Fatalf("first enroll: %d (%v)", code, body)
	}
	actorID, _ := body["actor_id"].(string)
	if actorID == "" {
		t.Fatalf("no actor_id in enroll response: %v", body)
	}

	// Simulate the admin-UI sign-out that stamped this machine.
	if err := c.registry.MarkActorReauthRequired(context.Background(), actorID); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if got := reauthStamp(t, c, actorID); got == "" {
		t.Fatal("marker not set by MarkActorReauthRequired")
	}

	// Re-running `txco login` re-enrolls the SAME key → same actor, and the
	// verified id_token is the fresh identity-provider login that clears it.
	code, body = env.enroll(t, oauthEnrollRequest{
		IDToken:   env.token(t, "user-1"),
		PublicKey: pubB64,
		Label:     "matt@macbook",
	})
	if code != http.StatusOK {
		t.Fatalf("re-enroll: %d (%v)", code, body)
	}
	if got := body["actor_id"]; got != actorID {
		t.Fatalf("re-enroll actor_id = %v, want %s (same key must reuse its actor)", got, actorID)
	}
	if got := reauthStamp(t, c, actorID); got != "" {
		t.Errorf("reauth_required_at = %q after re-enrollment, want cleared", got)
	}
}

// TestActorRevokeRevokesBrowserSessions: revoking an actor must also kill the
// browser sessions it already minted. verifyCookie checks only the session
// row and never re-checks the actor, so without this a revoked user keeps a
// working admin cookie for the full 7-day session TTL.
func TestActorRevokeRevokesBrowserSessions(t *testing.T) {
	c, srv := browserAuthTestServer(t, auth.ModeSigned)
	seedTenantActor(t, c, "actor_target", "tnt_default", "default", []string{"opstack:*:read"})
	token := mintBootstrap(t, c, "actor_target", "tnt_default", []string{"opstack:*:read"})
	cookie := exchangeForCookie(t, srv.URL, token)

	// Sanity: the cookie works before the revoke.
	resp := getWithCookie(t, srv.URL+"/auth/browser/session", cookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-revoke session GET: %d, want 200", resp.StatusCode)
	}

	// Revoke as a DIFFERENT actor — the handler refuses self-revoke.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/auth/actors/actor_target/revoke", nil)
	r = mux.SetURLVars(r, map[string]string{"tenant": "default", "actorID": "actor_target"})
	r = r.WithContext(auth.WithContext(r.Context(), &auth.Context{
		Source:       "signed",
		ActorID:      "actor_admin",
		TenantID:     "tnt_default",
		TenantSlug:   "default",
		Capabilities: []string{"actor:*:revoke"},
	}))
	c.handleRevokeActor(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke actor: %d %s", w.Code, w.Body.String())
	}

	var live int
	if err := c.pu.AuthDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM browser_sessions WHERE actor_id = ? AND revoked_at IS NULL`,
		"actor_target").Scan(&live); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if live != 0 {
		t.Errorf("%d live browser session(s) remain for a revoked actor, want 0", live)
	}
}

// TestReauthMarkerUnknownActor: the marker check gates a call that is already
// authenticated, so a missing actor row must read as "nothing blocking you"
// rather than an error that would 500 every bootstrap.
func TestReauthMarkerUnknownActor(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	required, err := c.registry.ActorReauthRequired(context.Background(), "actor_nope")
	if err != nil {
		t.Fatalf("ActorReauthRequired on unknown actor: %v", err)
	}
	if required {
		t.Error("unknown actor reported as needing re-auth")
	}
}
