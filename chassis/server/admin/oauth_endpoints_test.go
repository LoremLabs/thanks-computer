package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

const (
	testOAuthIssuer = "https://issuer.test"
	testOAuthAud    = "thanks-computer"
)

type oauthTestEnv struct {
	c       *Controller
	signKey jwk.Key
}

// wireOAuth equips a Controller for /auth/oauth/enroll: an RSA signing key
// whose public half becomes the controller's JWKS, plus the asserted issuer +
// audience. No network — the key set is in-process. Returns the private
// signing key.
func wireOAuth(t *testing.T, c *Controller) jwk.Key {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	pub, err := jwk.FromRaw(rsaKey.Public())
	if err != nil {
		t.Fatalf("jwk pub: %v", err)
	}
	_ = pub.Set(jwk.KeyIDKey, "test-key-1")
	_ = pub.Set(jwk.AlgorithmKey, jwa.RS256)
	set := jwk.NewSet()
	_ = set.AddKey(pub)

	priv, err := jwk.FromRaw(rsaKey)
	if err != nil {
		t.Fatalf("jwk priv: %v", err)
	}
	_ = priv.Set(jwk.KeyIDKey, "test-key-1")
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256)

	c.oauthIssuer = testOAuthIssuer
	c.oauthAudience = testOAuthAud
	c.oauthJWKS = set
	return priv
}

// newOAuthTestEnv builds a fleet-disabled Controller wired for enroll.
func newOAuthTestEnv(t *testing.T) *oauthTestEnv {
	t.Helper()
	c := newTestController(t, config.Config{CloudChassisURL: "https://chassis.test"})
	return &oauthTestEnv{c: c, signKey: wireOAuth(t, c)}
}

func (e *oauthTestEnv) token(t *testing.T, sub string) string {
	return e.tokenWith(t, sub, testOAuthAud, time.Now().Add(time.Hour))
}

func (e *oauthTestEnv) tokenWith(t *testing.T, sub, aud string, exp time.Time) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(testOAuthIssuer).
		Subject(sub).
		IssuedAt(time.Now()).
		Expiration(exp)
	if aud != "" {
		b = b.Audience([]string{aud})
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, e.signKey))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

func (e *oauthTestEnv) enroll(t *testing.T, body oauthEnrollRequest) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/auth/oauth/enroll", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	e.c.handleOAuthEnroll(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func newEd25519B64(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func detailStr(body map[string]any, key string) string {
	d, _ := body["detail"].(map[string]any)
	s, _ := d[key].(string)
	return s
}

// --- gating ----------------------------------------------------------------

func TestOAuthEnrollDisabled(t *testing.T) {
	c := newTestController(t, config.Config{}) // no issuer ⇒ endpoint disabled
	req := httptest.NewRequest(http.MethodPost, "/auth/oauth/enroll", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	c.handleOAuthEnroll(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled: status = %d, want 404", w.Code)
	}
}

// --- token validation ------------------------------------------------------

func TestOAuthEnrollBadToken(t *testing.T) {
	e := newOAuthTestEnv(t)
	code, _ := e.enroll(t, oauthEnrollRequest{IDToken: "not.a.jwt", PublicKey: newEd25519B64(t), TenantSlug: "matt"})
	if code != http.StatusUnauthorized {
		t.Fatalf("bad token: status = %d, want 401", code)
	}
}

func TestOAuthEnrollExpiredToken(t *testing.T) {
	e := newOAuthTestEnv(t)
	tok := e.tokenWith(t, "email:matt@example.com", testOAuthAud, time.Now().Add(-time.Hour))
	code, _ := e.enroll(t, oauthEnrollRequest{IDToken: tok, PublicKey: newEd25519B64(t), TenantSlug: "matt"})
	if code != http.StatusUnauthorized {
		t.Fatalf("expired token: status = %d, want 401", code)
	}
}

func TestOAuthEnrollWrongAudience(t *testing.T) {
	e := newOAuthTestEnv(t)
	tok := e.tokenWith(t, "email:matt@example.com", "some-other-client", time.Now().Add(time.Hour))
	code, _ := e.enroll(t, oauthEnrollRequest{IDToken: tok, PublicKey: newEd25519B64(t), TenantSlug: "matt"})
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong aud: status = %d, want 401", code)
	}
}

// --- first-enroll slug flow ------------------------------------------------

func TestOAuthEnrollFirstNoSlug(t *testing.T) {
	e := newOAuthTestEnv(t)
	code, body := e.enroll(t, oauthEnrollRequest{
		IDToken:   e.token(t, "email:matt@example.com"),
		PublicKey: newEd25519B64(t),
	})
	if code != http.StatusConflict {
		t.Fatalf("no slug: status = %d, want 409", code)
	}
	if body["error"] != "tenant_slug_required" {
		t.Fatalf("error = %v, want tenant_slug_required", body["error"])
	}
	if got := detailStr(body, "suggested_tenant_slug"); got != "matt" {
		t.Fatalf("suggested_tenant_slug = %q, want matt", got)
	}
	// Nothing was created.
	if _, err := e.c.registry.LookupOIDCSubject(context.Background(), testOAuthIssuer, "email:matt@example.com"); err == nil {
		t.Fatalf("a mapping was created on a slug-less first enroll")
	}
}

func TestOAuthEnrollFirstWithSlug(t *testing.T) {
	e := newOAuthTestEnv(t)
	pk := newEd25519B64(t)
	code, body := e.enroll(t, oauthEnrollRequest{
		IDToken:    e.token(t, "email:matt@example.com"),
		PublicKey:  pk,
		Label:      "matt@macbook",
		TenantSlug: "matt",
	})
	if code != http.StatusOK {
		t.Fatalf("with slug: status = %d body=%v", code, body)
	}
	if body["chassis_url"] != "https://chassis.test" {
		t.Fatalf("chassis_url = %v", body["chassis_url"])
	}
	if body["tenant_slug"] != "matt" {
		t.Fatalf("tenant_slug = %v, want matt", body["tenant_slug"])
	}
	if s, _ := body["actor_id"].(string); !strings.HasPrefix(s, "actor_") {
		t.Fatalf("actor_id = %v", body["actor_id"])
	}
	caps, _ := body["capabilities"].([]any)
	if len(caps) != 5 {
		t.Fatalf("capabilities = %v, want 5 owner caps", body["capabilities"])
	}
	// A tenant owner must be able to manage their own tenant's secrets and to
	// read their own tenant's KV (e.g. list a namespace via the admin API).
	hasSecret, hasKV := false, false
	for _, cp := range caps {
		switch s, _ := cp.(string); s {
		case "secret:*:*":
			hasSecret = true
		case "kv:*:*":
			hasKV = true
		}
	}
	if !hasSecret {
		t.Fatalf("owner caps missing secret:*:*: %v", body["capabilities"])
	}
	if !hasKV {
		t.Fatalf("owner caps missing kv:*:*: %v", body["capabilities"])
	}
	// The mapping + tenant now exist.
	tid, err := e.c.registry.LookupOIDCSubject(context.Background(), testOAuthIssuer, "email:matt@example.com")
	if err != nil {
		t.Fatalf("mapping missing after enroll: %v", err)
	}
	if tr, err := e.c.tenants.Lookup(context.Background(), tid); err != nil || tr.Slug != "matt" {
		t.Fatalf("tenant lookup: tr=%v err=%v", tr, err)
	}
}

func TestOAuthEnrollReservedSlug(t *testing.T) {
	e := newOAuthTestEnv(t)
	code, body := e.enroll(t, oauthEnrollRequest{
		IDToken:    e.token(t, "github:mankins"),
		PublicKey:  newEd25519B64(t),
		TenantSlug: "_sys",
	})
	if code != http.StatusConflict || body["error"] != "tenant_slug_invalid" {
		t.Fatalf("reserved slug: status=%d error=%v, want 409 tenant_slug_invalid", code, body["error"])
	}
	if got := detailStr(body, "suggested_tenant_slug"); got != "mankins" {
		t.Fatalf("suggested = %q, want mankins (from github:mankins)", got)
	}
}

func TestOAuthEnrollTakenSlug(t *testing.T) {
	e := newOAuthTestEnv(t)
	// First identity claims "matt".
	if code, body := e.enroll(t, oauthEnrollRequest{
		IDToken: e.token(t, "email:matt@a.com"), PublicKey: newEd25519B64(t), TenantSlug: "matt",
	}); code != http.StatusOK {
		t.Fatalf("seed enroll: status=%d body=%v", code, body)
	}
	// A different identity asks for the same slug → 409 + a fresh suggestion.
	code, body := e.enroll(t, oauthEnrollRequest{
		IDToken: e.token(t, "email:matt@b.com"), PublicKey: newEd25519B64(t), TenantSlug: "matt",
	})
	if code != http.StatusConflict || body["error"] != "tenant_slug_taken" {
		t.Fatalf("taken slug: status=%d error=%v, want 409 tenant_slug_taken", code, body["error"])
	}
	if got := detailStr(body, "suggested_tenant_slug"); got != "matt-2" {
		t.Fatalf("fresh suggestion = %q, want matt-2", got)
	}
}

// --- idempotency -----------------------------------------------------------

func TestOAuthEnrollIdempotentSameKey(t *testing.T) {
	e := newOAuthTestEnv(t)
	pk := newEd25519B64(t)
	sub := "email:matt@example.com"
	_, first := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, sub), PublicKey: pk, TenantSlug: "matt"})
	code, second := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, sub), PublicKey: pk, TenantSlug: "ignored"})
	if code != http.StatusOK {
		t.Fatalf("re-enroll: status = %d", code)
	}
	if first["actor_id"] != second["actor_id"] || first["key_id"] != second["key_id"] {
		t.Fatalf("idempotent re-enroll minted a new principal: %v vs %v", first, second)
	}
	if second["tenant_slug"] != "matt" {
		t.Fatalf("tenant_slug = %v, want matt (request slug must be ignored once mapped)", second["tenant_slug"])
	}
}

func TestOAuthEnrollSecondMachineNewKey(t *testing.T) {
	e := newOAuthTestEnv(t)
	sub := "email:matt@example.com"
	_, first := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, sub), PublicKey: newEd25519B64(t), TenantSlug: "matt"})
	code, second := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, sub), PublicKey: newEd25519B64(t)})
	if code != http.StatusOK {
		t.Fatalf("second machine: status = %d body=%v", code, second)
	}
	if first["actor_id"] == second["actor_id"] {
		t.Fatalf("second machine should mint a new actor, got same %v", first["actor_id"])
	}
	if second["tenant_slug"] != "matt" {
		t.Fatalf("second machine landed in tenant %v, want matt", second["tenant_slug"])
	}
}

// --- fleet-sync producer adherence -----------------------------------------

func TestOAuthEnrollEmitsTenantCreatedEvent(t *testing.T) {
	c := newTestController(t, config.Config{CloudChassisURL: "https://chassis.test", FeedSink: "file"})
	withAStore(t, c)
	e := &oauthTestEnv{c: c, signKey: wireOAuth(t, c)}

	code, body := e.enroll(t, oauthEnrollRequest{
		IDToken:    e.token(t, "email:matt@example.com"),
		PublicKey:  newEd25519B64(t),
		TenantSlug: "matt",
	})
	if code != http.StatusOK {
		t.Fatalf("enroll status = %d body=%v", code, body)
	}

	// Creating a tenant must queue a tenant.created outbox event for it, so a
	// multi-worker fleet upserts the tenants row and can route its hostnames.
	tr, err := c.tenants.LookupBySlug(context.Background(), "matt")
	if err != nil || tr == nil {
		t.Fatalf("tenant lookup: tr=%v err=%v", tr, err)
	}
	var gotType, gotTenant string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT event_type, tenant_id FROM control_events_outbox WHERE event_type = ?`,
		controlevent.TypeTenantCreated).Scan(&gotType, &gotTenant); err != nil {
		t.Fatalf("expected a tenant.created outbox row: %v", err)
	}
	if gotTenant != tr.TenantID {
		t.Fatalf("outbox tenant_id = %q, want %q", gotTenant, tr.TenantID)
	}
}

// --- redaction -------------------------------------------------------------

func TestOAuthEnrollDoesNotLogIDToken(t *testing.T) {
	e := newOAuthTestEnv(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e.c.pu.Logger = zap.New(core)

	idTok := e.token(t, "email:matt@example.com")
	code, _ := e.enroll(t, oauthEnrollRequest{IDToken: idTok, PublicKey: newEd25519B64(t), TenantSlug: "matt"})
	if code != http.StatusOK {
		t.Fatalf("enroll status = %d", code)
	}

	for _, entry := range logs.All() {
		blob := entry.Message
		for k, v := range entry.ContextMap() {
			blob += fmt.Sprintf(" %s=%v", k, v)
		}
		if strings.Contains(blob, idTok) {
			t.Fatalf("id_token leaked into logs: %s", blob)
		}
	}
	found := false
	for _, entry := range logs.FilterMessage("oauth-enrolled actor").All() {
		m := entry.ContextMap()
		if m["actor_id"] != nil && m["tenant_id"] != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an 'oauth-enrolled actor' log carrying actor_id + tenant_id")
	}
}
