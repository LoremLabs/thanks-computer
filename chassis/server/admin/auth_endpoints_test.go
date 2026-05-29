package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// withRouter wires the controller's protected routes through the real
// auth middleware and the gorilla/mux variable extractor. Tests use
// this to exercise the full middleware → policy → handler path that
// production sees.
func withRouter(t *testing.T, c *Controller, mode auth.AuthMode) http.Handler {
	t.Helper()
	r := mux.NewRouter()
	r.HandleFunc("/healthz", c.handleHealth).Methods(http.MethodGet)
	r.HandleFunc("/auth/dev/enroll", c.handleDevEnroll).Methods(http.MethodPost)
	r.HandleFunc("/auth/invitations/consume", c.handleConsumeInvitation).Methods(http.MethodPost)

	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return auth.Middleware(auth.Config{
			Mode:      mode,
			BasicUser: c.pu.Conf.AdminUser,
			BasicPass: c.pu.Conf.AdminPass,
			Registry:  c.registry,
			Verifier:  c.verifier,
			Nonces:    c.nonces,
			Skew:      5 * time.Minute,
		}, next)
	})

	// Chassis-wide routes that don't go through the tenant subrouter.
	protected.HandleFunc("/auth/whoami", c.handleWhoami).Methods(http.MethodGet)
	protected.HandleFunc("/auth/keys/{keyID}/revoke", c.handleRevokeKey).Methods(http.MethodPost)

	// Tenant-scoped subrouter: post-phase-2 production shape. The
	// tenant middleware resolves slug → tenant_id and (for signed
	// non-super-admin actors) replaces ac.Capabilities with the
	// caller's membership for that tenant. Tests target
	// /v1/tenants/default/<suffix>.
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/ops", c.handleListOps).Methods(http.MethodGet)
	tenantR.HandleFunc("/stacks/{name:.+}/draft", c.handleCreateDraft).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/actors", c.handleListActors).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/actors/{actorID}/revoke", c.handleRevokeActor).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/invitations", c.handleListInvitations).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/invitations", c.handleCreateInvitation).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/invitations/{invID}/revoke", c.handleRevokeInvitation).Methods(http.MethodPost)
	return r
}

func enroll(t *testing.T, srv *httptest.Server, secret string) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	body, _ := json.Marshal(devEnrollRequest{
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
		Label:        "test box",
		Kind:         "developer-cli",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/dev/enroll", bytes.NewReader(body))
	req.Header.Set("X-Txco-Enroll-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("enroll status=%d body=%s", resp.StatusCode, out)
	}
	var er devEnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if er.ActorID == "" || er.KeyID == "" {
		t.Fatalf("enroll didn't return ids: %+v", er)
	}
	return er.KeyID, priv
}

// signedGET signs a GET request with the supplied keyID/private key
// and returns the response.
func signedGET(t *testing.T, srv *httptest.Server, path, keyID string, priv ed25519.PrivateKey) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err := signature.NewSigner().Sign(req, signature.ActorPrivateKey{KeyID: keyID, PrivateKey: priv}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

func signedPOST(t *testing.T, srv *httptest.Server, path, keyID string, priv ed25519.PrivateKey, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(body)))
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/json")
	if err := signature.NewSigner().Sign(req, signature.ActorPrivateKey{KeyID: keyID, PrivateKey: priv}); err != nil {
		t.Fatalf("sign: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestDevEnrollAndWhoamiRoundtrip is the canonical "bootstrap-local"
// flow: enroll a keypair, then sign a /auth/whoami request and confirm
// the chassis sees the actor + admin:all capability.
func TestDevEnrollAndWhoamiRoundtrip(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "shhh",
		Environment:         "dev",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	keyID, priv := enroll(t, srv, "shhh")

	resp := signedGET(t, srv, "/auth/whoami", keyID, priv)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("whoami status=%d body=%s", resp.StatusCode, out)
	}
	var wr whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if wr.Source != "signed" {
		t.Errorf("got source=%q, want signed", wr.Source)
	}
	if wr.KeyID != keyID {
		t.Errorf("got keyID=%q, want %q", wr.KeyID, keyID)
	}
	// Phase 8b: chassis-wide capabilities are no longer populated
	// from actor_capabilities. The actor's admin:all lives in their
	// default-tenant membership instead. This test enrolls with an
	// explicit operator-supplied secret (not first-boot autoGen),
	// so super_admin stays false — only the membership row is set.
	if wr.SuperAdmin {
		t.Errorf("explicit-secret enrolment should NOT yield super_admin")
	}
	if len(wr.Memberships) != 1 {
		t.Fatalf("expected 1 membership; got %v", wr.Memberships)
	}
	if wr.Memberships[0].TenantSlug != "default" ||
		len(wr.Memberships[0].Capabilities) != 1 ||
		wr.Memberships[0].Capabilities[0] != "admin:all" {
		t.Errorf("membership = %+v, want default:[admin:all]", wr.Memberships[0])
	}
}

// TestDevEnrollRequiresSecret guards the gating: a missing or wrong
// X-Txco-Enroll-Secret header is 401.
func TestDevEnrollRequiresSecret(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "right",
		Environment:         "dev",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(devEnrollRequest{
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
	})

	cases := []struct {
		name   string
		secret string
		want   int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "wrong", http.StatusUnauthorized},
		{"correct", "right", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/dev/enroll", bytes.NewReader(body))
			if tc.secret != "" {
				req.Header.Set("X-Txco-Enroll-Secret", tc.secret)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestDevEnrollAllowedInProd — the chassis no longer refuses dev
// enrolment in prod. The safety boundary is "registry already has an
// actor" (burn-after-use for auto-generated secrets) or simply the
// operator's decision not to set an explicit secret. An operator who
// explicitly sets --auth-dev-enroll-secret in prod is taken at their
// word; this is the recovery path.
func TestDevEnrollAllowedInProd(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "shhh",
		Environment:         "prod",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(devEnrollRequest{
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/dev/enroll", bytes.NewReader(body))
	req.Header.Set("X-Txco-Enroll-Secret", "shhh")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Errorf("got %d, want 200 (prod with explicit secret should enroll); body=%s", resp.StatusCode, out)
	}
}

// TestSignedDraftRoundtrip is the end-to-end happy path: enroll, then
// sign POST /v1/tenants/default/stacks/<name>/draft. The middleware
// verifies + checks capability opstack:*:update; the handler creates
// the draft. Replaces the legacy /ops/import smoke now that the flat
// bulk-upsert path is retired.
func TestSignedDraftRoundtrip(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "shhh",
		Environment:         "dev",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	keyID, priv := enroll(t, srv, "shhh")
	body := []byte(`{"from":""}`)

	resp := signedPOST(t, srv, "/v1/tenants/default/stacks/demo/draft", keyID, priv, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("draft status=%d body=%s", resp.StatusCode, out)
	}

	var count int
	_ = c.pu.RuntimeDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM stack_versions`).Scan(&count)
	if count != 1 {
		t.Errorf("stack_versions count = %d, want 1", count)
	}
}

// TestRevokedKeyBlocksRequest: after revoking a key, signed requests
// using it are rejected with the revoked_key error code.
func TestRevokedKeyBlocksRequest(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "shhh",
		Environment:         "dev",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	keyID, priv := enroll(t, srv, "shhh")

	// First whoami succeeds.
	r1 := signedGET(t, srv, "/auth/whoami", keyID, priv)
	_ = r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("pre-revoke whoami status=%d", r1.StatusCode)
	}

	// Revoke the key via the admin endpoint (signed with the same key
	// — actor:revoke is part of admin:all).
	r2 := signedPOST(t, srv, "/auth/keys/"+keyID+"/revoke", keyID, priv, nil)
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", r2.StatusCode)
	}

	// Subsequent whoami fails with revoked_key.
	r3 := signedGET(t, srv, "/auth/whoami", keyID, priv)
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-revoke whoami status=%d, want 401", r3.StatusCode)
	}
	var er map[string]string
	_ = json.NewDecoder(r3.Body).Decode(&er)
	if er["error"] != signature.ErrRevokedKey {
		t.Errorf("got error=%q, want %q", er["error"], signature.ErrRevokedKey)
	}
}

// TestUnsignedRequestRejectedInSignedMode is the basic guarantee that
// `--auth-mode=signed` actually keeps unsigned callers out.
func TestUnsignedRequestRejectedInSignedMode(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "signed",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/tenants/default/ops")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got status=%d, want 401", resp.StatusCode)
	}
	var er map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er["error"] != signature.ErrMissingSignatureHeaders {
		t.Errorf("got error=%q, want %q", er["error"], signature.ErrMissingSignatureHeaders)
	}
}

// TestBothModeAllowsBasicAtWhoami covers the user-flagged debugging
// affordance: in `both` mode, a Basic caller can hit /auth/whoami and
// see their synthetic admin:all context.
func TestBothModeAllowsBasicAtWhoami(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "both",
		AdminUser:     "alice",
		AdminPass:     "secret",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeBoth))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/auth/whoami", nil)
	req.SetBasicAuth("alice", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, out)
	}
	var wr whoamiResponse
	_ = json.NewDecoder(resp.Body).Decode(&wr)
	if wr.Source != "basic" {
		t.Errorf("got source=%q, want basic", wr.Source)
	}
}
