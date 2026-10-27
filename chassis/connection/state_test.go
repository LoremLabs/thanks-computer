package connection

import (
	"bytes"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(bytes.Repeat([]byte{7}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStateSignVerify(t *testing.T) {
	s := testSigner(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	nonce, _ := newNonce()
	state, err := s.sign(stateClaims{Tenant: "acme", Stack: "loop", Provider: "github", Nonce: nonce, Expires: now.Add(FlowTTL).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.verify(state, now)
	if err != nil || c.Tenant != "acme" || c.Stack != "loop" || c.Provider != "github" || c.Nonce != nonce || c.KeyVersion != 1 {
		t.Fatalf("verify = %+v, %v", c, err)
	}
	if _, err := s.verify(state, now.Add(FlowTTL)); err != ErrStateExpired {
		t.Errorf("expired: %v", err)
	}
	// Tampered, truncated, another key, another version: all bad.
	for name, bad := range map[string]string{
		"flipped":   state[:len(state)-2] + "AA",
		"truncated": state[:len(state)/2],
		"empty":     "",
		"too long":  state + strings.Repeat("x", maxStateLen),
	} {
		if _, err := s.verify(bad, now); err != ErrBadState {
			t.Errorf("%s: %v, want ErrBadState", name, err)
		}
	}
	other, _ := NewSigner(bytes.Repeat([]byte{8}, 32), 1)
	if _, err := other.verify(state, now); err != ErrBadState {
		t.Error("another key must not verify")
	}
	v2, _ := NewSigner(bytes.Repeat([]byte{7}, 32), 2)
	if _, err := v2.verify(state, now); err != ErrBadState {
		t.Error("another key generation must not verify")
	}
	// The PKCE verifier is 43 characters of the unreserved alphabet,
	// stable for a nonce, different across nonces and keys.
	ver := s.verifier(nonce)
	if len(ver) != 43 || strings.ContainsAny(ver, "+/=") || ver != s.verifier(nonce) {
		t.Errorf("verifier %q", ver)
	}
	if other.verifier(nonce) == ver {
		t.Error("the verifier must depend on the key")
	}
	if len(challenge(ver)) != 43 {
		t.Error("challenge")
	}
}

func TestOrigins(t *testing.T) {
	for raw, want := range map[string]string{
		"https://api.github.com":       "https://api.github.com",
		"https://API.GitHub.com:443/":  "https://api.github.com",
		"https://api.github.com:8443":  "https://api.github.com:8443",
		"http://localhost:9090":        "http://localhost:9090",
		"http://127.0.0.1:9090":        "http://127.0.0.1:9090",
		"https://bücher.example":       "https://xn--bcher-kva.example",
		"https://[::1]:8443":           "https://[::1]:8443",
		"HTTPS://api.github.com":       "https://api.github.com",
		"https://api.github.com/":      "https://api.github.com",
		"https://example.com:443":      "https://example.com",
		"http://localhost":             "http://localhost",
		"https://sub.localhost.evil":   "https://sub.localhost.evil",
		"https://api.github.com:00443": "https://api.github.com:00443",
	} {
		o, err := ParseOrigin(raw)
		if err != nil {
			t.Errorf("ParseOrigin(%q): %v", raw, err)
			continue
		}
		if o.String() != want {
			t.Errorf("ParseOrigin(%q) = %s, want %s", raw, o, want)
		}
	}
	for _, raw := range []string{
		"api.github.com", "ftp://x", "https://api.github.com/v3", "https://api.github.com?x=1", "https://api.github.com#f",
		"https://u:p@api.github.com", "http://api.github.com", "http://10.0.0.1", "https://", "",
	} {
		if _, err := ParseOrigin(raw); err == nil {
			t.Errorf("ParseOrigin(%q) parsed, want an error", raw)
		}
	}
	u, _ := url.Parse("https://API.github.com/repos/x?y=1")
	o, err := OriginOf(u)
	if err != nil || o.String() != "https://api.github.com" {
		t.Errorf("OriginOf = %s, %v", o, err)
	}
	// A port the declaration did not name is another origin.
	decl, _ := ParseOrigin("https://api.github.com")
	u, _ = url.Parse("https://api.github.com:8443/x")
	if got, _ := OriginOf(u); got == decl {
		t.Error("another port must not match")
	}
	if !Loopback("localhost") || !Loopback("127.0.0.1") || !Loopback("::1") || !Loopback("a.localhost") || Loopback("10.0.0.1") || Loopback("localhost.evil") {
		t.Error("Loopback")
	}
}

func TestMargin(t *testing.T) {
	p := &Provider{Refresh: Refresh{Margin: Margin{Fraction: 0.2, FloorS: 60, CeilingS: 900}}}
	for life, want := range map[int64]time.Duration{
		0:     0,
		30:    15 * time.Second, // half-life beats the floor
		100:   50 * time.Second,
		600:   120 * time.Second, // a fifth
		28800: 900 * time.Second, // the ceiling
		200:   60 * time.Second,  // the floor
	} {
		if got := margin(p, life); got != want {
			t.Errorf("margin(%d) = %s, want %s", life, got, want)
		}
	}
}

func TestSetCookie(t *testing.T) {
	line := setCookie("v", "ranch.example", 600)
	if !strings.HasPrefix(line, CookieName+"=v; ") || !strings.Contains(line, "; Secure") || !strings.Contains(line, "HttpOnly") || !strings.Contains(line, "SameSite=Lax") {
		t.Errorf("cookie %q", line)
	}
	if strings.Contains(setCookie("v", "localhost:8080", 600), "Secure") {
		t.Error("no Secure on a loopback host")
	}
	if !strings.Contains(setCookie("", "ranch.example", 0), "Max-Age=0") {
		t.Error("clearing")
	}
}

func TestCheckRedirectURI(t *testing.T) {
	if _, err := checkRedirectURI("https://ranch.example/connect/github/callback", "ranch.example", "github"); err != nil {
		t.Error(err)
	}
	if _, err := checkRedirectURI("https://ranch.example/connect/github", "RANCH.example", "github"); err != nil {
		t.Error(err)
	}
	if _, err := checkRedirectURI("http://localhost:8080/connect/github/cb", "localhost:8080", "github"); err != nil {
		t.Error(err)
	}
	for name, in := range map[string][3]string{
		"other host":     {"https://evil.example/connect/github/callback", "ranch.example", "github"},
		"http":           {"http://ranch.example/connect/github/callback", "ranch.example", "github"},
		"no provider":    {"https://ranch.example/connect/callback", "ranch.example", "github"},
		"other provider": {"https://ranch.example/connect/slack/callback", "ranch.example", "github"},
		"query":          {"https://ranch.example/connect/github/callback?x=1", "ranch.example", "github"},
		"relative":       {"/connect/github/callback", "ranch.example", "github"},
	} {
		if _, err := checkRedirectURI(in[0], in[1], in[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
