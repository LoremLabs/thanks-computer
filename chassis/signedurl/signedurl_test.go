package signedurl

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T, fill byte, ver int) *Signer {
	t.Helper()
	s, err := New(bytes.Repeat([]byte{fill}, 32), ver)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := testSigner(t, 1, 1)
	now := time.Unix(1_800_000_000, 0)
	in := Claims{Tenant: "acme", Collection: "dc_1", Resource: "dr_1", SHA256: strings.Repeat("ab", 32), Expires: now.Add(time.Minute).Unix()}
	tok, err := s.Sign(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(tok, "/+=") {
		t.Errorf("token is not URL-path safe: %s", tok)
	}
	got, err := s.Verify(tok, now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	in.KeyVersion = 1
	if got != in {
		t.Errorf("claims = %+v, want %+v", got, in)
	}
	// The last second it is valid, and the first it is not.
	if _, err := s.Verify(tok, now.Add(59*time.Second)); err != nil {
		t.Errorf("at expiry-1s: %v", err)
	}
	if _, err := s.Verify(tok, now.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Errorf("at expiry: %v, want ErrExpired", err)
	}
}

func TestVerifyRefusals(t *testing.T) {
	s := testSigner(t, 1, 1)
	now := time.Unix(1_800_000_000, 0)
	claims := Claims{Tenant: "acme", Collection: "dc_1", Resource: "dr_1", SHA256: "aa", Expires: now.Add(time.Minute).Unix()}
	tok, _ := s.Sign(claims)
	parts := strings.Split(tok, ".")

	// A forger who rewrites the claims keeps the old MAC.
	evil := claims
	evil.Tenant, evil.KeyVersion = "victim", 1
	forged := func() string {
		other, _ := testSigner(t, 9, 1).Sign(evil)
		return strings.Split(other, ".")[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]
	}()

	for name, bad := range map[string]string{
		"empty":           "",
		"garbage":         "not-a-token",
		"two parts":       parts[0] + "." + parts[1],
		"four parts":      tok + ".x",
		"wrong version":   "v2." + parts[1] + "." + parts[2],
		"claims swapped":  forged,
		"mac truncated":   parts[0] + "." + parts[1] + "." + parts[2][:10],
		"mac not base64":  parts[0] + "." + parts[1] + ".!!!",
		"padded base64":   parts[0] + "." + parts[1] + "=." + parts[2],
		"oversized":       tok + strings.Repeat("A", maxTokenLen),
		"signed elsewise": func() string { o, _ := testSigner(t, 2, 1).Sign(claims); return o }(),
	} {
		if _, err := s.Verify(bad, now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	// Same key bytes, another master-key generation: the token names the
	// generation that signed it, and only that one accepts it.
	if _, err := testSigner(t, 1, 2).Verify(tok, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("other key generation: %v, want ErrInvalid", err)
	}

	// A genuine MAC over claims that are not complete is still refused.
	raw := base64.RawURLEncoding.EncodeToString([]byte(`{"t":"acme","e":9999999999,"k":1}`))
	signed := version + "." + raw
	hollow := signed + "." + base64.RawURLEncoding.EncodeToString(s.mac(signed))
	if _, err := s.Verify(hollow, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("incomplete claims: %v, want ErrInvalid", err)
	}
}

func TestSignRefusesIncompleteClaims(t *testing.T) {
	s := testSigner(t, 1, 1)
	for _, c := range []Claims{
		{},
		{Tenant: "a", Collection: "c", Resource: "r", SHA256: "h"},               // no expiry
		{Collection: "c", Resource: "r", SHA256: "h", Expires: 1},                // no tenant
		{Tenant: "a", Collection: "c", Resource: "r", Expires: 1},                // no content pin
		{Tenant: "a", Resource: "r", SHA256: "h", Expires: 1},                    // no collection
		{Tenant: "a", Collection: "c", SHA256: "h", Expires: 1, KeyVersion: 100}, // no resource
	} {
		if tok, err := s.Sign(c); err == nil {
			t.Errorf("Sign(%+v) = %q, want an error", c, tok)
		}
	}
	if _, err := New(make([]byte, 16), 1); err == nil {
		t.Error("a 16-byte key was accepted")
	}
}
