package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

func (e *oauthTestEnv) reauth(t *testing.T, body oauthReauthRequest) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/auth/oauth/reauth", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	e.c.handleOAuthReauth(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// enrolled enrolls a fresh key for sub into a new tenant named slug and
// returns the key and its actor.
func (e *oauthTestEnv) enrolled(t *testing.T, sub, slug string) (pubKey, actorID string) {
	t.Helper()
	pubKey = newEd25519B64(t)
	code, body := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, sub), PublicKey: pubKey, TenantSlug: slug})
	if code != http.StatusOK {
		t.Fatalf("enroll %s: status = %d body=%v", sub, code, body)
	}
	actorID, _ = body["actor_id"].(string)
	return pubKey, actorID
}

func (e *oauthTestEnv) signOut(t *testing.T, actorID string) {
	t.Helper()
	if err := e.c.registry.MarkActorReauthRequired(context.Background(), actorID); err != nil {
		t.Fatalf("mark: %v", err)
	}
}

func tenantCount(t *testing.T, c *Controller) int {
	t.Helper()
	var n int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM tenants`).Scan(&n); err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	return n
}

// TestOAuthReauthClearsTheMarker is the regression for the sign-out
// dead end: a machine that is already enrolled signs back in with the
// account that enrolled it, and the marker is gone — without enroll.
func TestOAuthReauthClearsTheMarker(t *testing.T) {
	e := newOAuthTestEnv(t)
	pk, actor := e.enrolled(t, "email:owner@example.com", "pony")
	e.signOut(t, actor)
	if reauthStamp(t, e.c, actor) == "" {
		t.Fatal("precondition: marker not set")
	}
	before := tenantCount(t, e.c)

	code, body := e.reauth(t, oauthReauthRequest{IDToken: e.token(t, "email:owner@example.com"), PublicKey: pk})
	if code != http.StatusOK {
		t.Fatalf("reauth: status = %d body=%v", code, body)
	}
	if body["actor_id"] != actor || body["tenant_slug"] != "pony" || body["signed_in_as"] != "email:owner@example.com" {
		t.Errorf("reauth body = %v", body)
	}
	if got := reauthStamp(t, e.c, actor); got != "" {
		t.Errorf("marker still set: %q", got)
	}
	if after := tenantCount(t, e.c); after != before {
		t.Errorf("reauth created tenants: %d → %d", before, after)
	}
}

// TestOAuthReauthSecondMachine — a second key the same account enrolled
// signs back in the same way; per-actor means per-machine.
func TestOAuthReauthSecondMachine(t *testing.T) {
	e := newOAuthTestEnv(t)
	_, first := e.enrolled(t, "github:owner", "pony")
	pk2 := newEd25519B64(t)
	code, body := e.enroll(t, oauthEnrollRequest{IDToken: e.token(t, "github:owner"), PublicKey: pk2})
	if code != http.StatusOK {
		t.Fatalf("second machine enroll: %d %v", code, body)
	}
	second, _ := body["actor_id"].(string)
	e.signOut(t, first)
	e.signOut(t, second)

	if code, body := e.reauth(t, oauthReauthRequest{IDToken: e.token(t, "github:owner"), PublicKey: pk2}); code != http.StatusOK {
		t.Fatalf("reauth second machine: %d %v", code, body)
	}
	if reauthStamp(t, e.c, second) != "" {
		t.Error("second machine's marker still set")
	}
	if reauthStamp(t, e.c, first) == "" {
		t.Error("the other machine's marker must be untouched")
	}
}

// TestOAuthReauthWrongAccountCreatesNothing — the two ways to pick the
// wrong account at the provider. Enroll would offer the first a new
// tenant and quietly add the second's tenant to this key's actor; reauth
// refuses both, says which account was used, and changes nothing.
func TestOAuthReauthWrongAccountCreatesNothing(t *testing.T) {
	e := newOAuthTestEnv(t)
	pk, actor := e.enrolled(t, "email:owner@example.com", "pony")
	_, _ = e.enrolled(t, "github:someone-else", "other") // an account with a space of its own
	e.signOut(t, actor)
	before := tenantCount(t, e.c)

	for sub, wantCode := range map[string]string{
		"email:stranger@example.com": "identity_not_enrolled", // no space at all
		"github:someone-else":        "identity_mismatch",     // a space, but not this key's
	} {
		code, body := e.reauth(t, oauthReauthRequest{IDToken: e.token(t, sub), PublicKey: pk})
		if code != http.StatusForbidden || body["error"] != wantCode {
			t.Errorf("%s: status = %d body=%v, want 403 %s", sub, code, body, wantCode)
		}
		if got := detailStr(body, "signed_in_as"); got != sub {
			t.Errorf("%s: signed_in_as = %q — the caller must be told which account they used", sub, got)
		}
		if strings.Contains(string(mustJSON(t, body)), "owner@example.com") {
			t.Errorf("%s: the response must not reveal the expected account: %v", sub, body)
		}
	}
	if reauthStamp(t, e.c, actor) == "" {
		t.Error("a refused reauth must leave the marker set")
	}
	if after := tenantCount(t, e.c); after != before {
		t.Errorf("a refused reauth created tenants: %d → %d", before, after)
	}
	other, err := e.c.tenants.LookupBySlug(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.registry.LoadMembership(context.Background(), actor, other.TenantID); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a refused reauth must not add the actor to the other account's tenant (err=%v)", err)
	}
}

func TestOAuthReauthUnknownKeyAndBadToken(t *testing.T) {
	e := newOAuthTestEnv(t)
	pk, _ := e.enrolled(t, "email:owner@example.com", "pony")

	if code, body := e.reauth(t, oauthReauthRequest{IDToken: e.token(t, "email:owner@example.com"), PublicKey: newEd25519B64(t)}); code != http.StatusNotFound || body["error"] != "key_not_enrolled" {
		t.Errorf("unknown key: %d %v", code, body)
	}
	if code, body := e.reauth(t, oauthReauthRequest{IDToken: "not-a-jwt", PublicKey: pk}); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d %v", code, body)
	}
}

func TestOAuthReauthDisabled(t *testing.T) {
	c := newTestController(t, config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/auth/oauth/reauth", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	c.handleOAuthReauth(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no issuer: status = %d", w.Code)
	}
}
