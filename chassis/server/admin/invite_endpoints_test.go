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
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// inviteAsAdmin runs the end-to-end "bootstrap an admin and have it
// mint an invitation" prelude that most invitation tests share. Returns
// the controller (so tests can poke the underlying DB), the test
// server, the admin's (keyID, priv) for further signed calls, and the
// raw invitation token.
func inviteAsAdmin(t *testing.T, label string) (*Controller, *httptest.Server, string, ed25519.PrivateKey, createInvitationResponse) {
	t.Helper()
	c := newTestController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "shhh",
		Environment:         "dev",
	})
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	keyID, priv := enroll(t, srv, "shhh")

	body, _ := json.Marshal(createInvitationRequest{Label: label})
	resp := signedPOST(t, srv, "/v1/tenants/default/auth/invitations", keyID, priv, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("create invite status=%d body=%s", resp.StatusCode, out)
	}
	var cr createInvitationResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	return c, srv, keyID, priv, cr
}

// TestCreateInvitationSigned — admin signs POST /auth/invitations,
// gets back a token + invitation_id + expiry. Spot-checks the token
// shape too (8 hyphen-separated words).
func TestCreateInvitationSigned(t *testing.T) {
	_, _, _, _, cr := inviteAsAdmin(t, "alice")

	if cr.InvitationID == "" || cr.Token == "" || cr.ExpiresAt == "" {
		t.Fatalf("missing fields: %+v", cr)
	}
	if got := len(bytes.Split([]byte(cr.Token), []byte("-"))); got != 8 {
		t.Errorf("token has %d hyphen-parts, want 8 (raw=%q)", got, cr.Token)
	}
}

// TestConsumeInvitationFullRoundtrip — admin creates an invitation,
// invitee redeems it on the unsigned endpoint and gets a fresh actor
// + key. Verify the response carries the invitee's own identity, not
// the admin's.
func TestConsumeInvitationFullRoundtrip(t *testing.T) {
	_, srv, _, _, cr := inviteAsAdmin(t, "alice")

	// Invitee generates their own keypair locally and POSTs.
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(consumeInvitationRequest{
		Token:        cr.Token,
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
		Label:        "alice laptop",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/invitations/consume", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("consume status=%d body=%s", resp.StatusCode, out)
	}
	var er devEnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatal(err)
	}
	if er.ActorID == "" || er.KeyID == "" {
		t.Errorf("missing ids: %+v", er)
	}
	// Phase-6: invitation caps are canonicalised at write time, so
	// the legacy "admin:all" the inviter passed gets stored as
	// "*:*:*" — the matcher treats them as equivalent. Either spelling
	// would do; we expect the canonical form here so the test pins
	// the actual storage shape.
	if len(er.Capabilities) != 1 || er.Capabilities[0] != "*:*:*" {
		t.Errorf("got caps=%v, want [*:*:*]", er.Capabilities)
	}
}

// TestConsumeInvitationBurnedOnSecondUse — the same token can't be
// redeemed twice. Both attempts get the same opaque 404.
func TestConsumeInvitationBurnedOnSecondUse(t *testing.T) {
	_, srv, _, _, cr := inviteAsAdmin(t, "alice")

	consume := func() *http.Response {
		pub, _, _ := ed25519.GenerateKey(nil)
		body, _ := json.Marshal(consumeInvitationRequest{
			Token:        cr.Token,
			PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
			Algorithm:    "ed25519",
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/invitations/consume", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r1 := consume()
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first consume status=%d", r1.StatusCode)
	}
	r2 := consume()
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusUnauthorized {
		out, _ := io.ReadAll(r2.Body)
		t.Errorf("second consume status=%d want 401; body=%s", r2.StatusCode, out)
	}
}

// TestRevokeInvitationBlocksConsume — admin revokes a fresh
// invitation; the same token then fails consume with 401.
func TestRevokeInvitationBlocksConsume(t *testing.T) {
	_, srv, keyID, priv, cr := inviteAsAdmin(t, "bob")

	// Revoke (signed; admin:all covers actor:revoke).
	r := signedPOST(t, srv, "/v1/tenants/default/auth/invitations/"+cr.InvitationID+"/revoke", keyID, priv, nil)
	_ = r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", r.StatusCode)
	}

	// Try to consume.
	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(consumeInvitationRequest{
		Token:        cr.Token,
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/invitations/consume", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		out, _ := io.ReadAll(resp.Body)
		t.Errorf("consume after revoke status=%d want 401; body=%s", resp.StatusCode, out)
	}
}

// TestListInvitationsStatus — exercises the status derivation:
// active, consumed, revoked, expired all appear correctly in GET
// /auth/invitations.
func TestListInvitationsStatus(t *testing.T) {
	c, srv, keyID, priv, alice := inviteAsAdmin(t, "alice")

	// Mint two more and put them in different states. We hit the
	// endpoint for the "consumed" one, then revoke another, then
	// backdate one via direct SQL.
	body, _ := json.Marshal(createInvitationRequest{Label: "bob"})
	r := signedPOST(t, srv, "/v1/tenants/default/auth/invitations", keyID, priv, body)
	defer r.Body.Close()
	var bob createInvitationResponse
	_ = json.NewDecoder(r.Body).Decode(&bob)

	body, _ = json.Marshal(createInvitationRequest{Label: "carol"})
	r = signedPOST(t, srv, "/v1/tenants/default/auth/invitations", keyID, priv, body)
	defer r.Body.Close()
	var carol createInvitationResponse
	_ = json.NewDecoder(r.Body).Decode(&carol)

	// Consume alice's.
	pub, _, _ := ed25519.GenerateKey(nil)
	cb, _ := json.Marshal(consumeInvitationRequest{
		Token: alice.Token, PublicKeyB64: base64.StdEncoding.EncodeToString(pub), Algorithm: "ed25519"})
	cr, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/invitations/consume", bytes.NewReader(cb))
	cr.Header.Set("Content-Type", "application/json")
	cresp, err := http.DefaultClient.Do(cr)
	if err != nil {
		t.Fatal(err)
	}
	_ = cresp.Body.Close()

	// Revoke bob's.
	rr := signedPOST(t, srv, "/v1/tenants/default/auth/invitations/"+bob.InvitationID+"/revoke", keyID, priv, nil)
	_ = rr.Body.Close()

	// Backdate carol's expiry directly so status derives as expired.
	// We reach into the underlying DB through the controller's
	// registry (test-only).
	if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
		`UPDATE actor_invitations SET expires_at = '2000-01-01T00:00:00Z' WHERE invitation_id = ?`,
		carol.InvitationID); err != nil {
		t.Fatalf("backdate carol: %v", err)
	}

	// Fetch the list.
	resp := signedGET(t, srv, "/v1/tenants/default/auth/invitations", keyID, priv)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		t.Fatalf("list status=%d body=%s", resp.StatusCode, out)
	}
	var lr listInvitationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, v := range lr.Invitations {
		statuses[v.Label] = v.Status
	}
	if statuses["alice"] != "consumed" {
		t.Errorf("alice status=%q, want consumed", statuses["alice"])
	}
	if statuses["bob"] != "revoked" {
		t.Errorf("bob status=%q, want revoked", statuses["bob"])
	}
	if statuses["carol"] != "expired" {
		t.Errorf("carol status=%q, want expired", statuses["carol"])
	}
}
