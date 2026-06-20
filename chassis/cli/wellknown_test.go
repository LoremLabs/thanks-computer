package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
)

// a real ssh-ed25519 public key line (the hello-world package signing key).
const testSigningPubkey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICO6UwPb2RVtKuUSnja04i/nmyOf0DJHUoGWXI/O+S8a"

func TestParseSigningKeys(t *testing.T) {
	t.Run("valid doc → registry-scoped keys", func(t *testing.T) {
		doc := `{"keys":[{"name":"txco","pubkey":"` + testSigningPubkey + `"}]}`
		keys, err := parseSigningKeys([]byte(doc), "registry.thanks.computer")
		if err != nil {
			t.Fatalf("parseSigningKeys: %v", err)
		}
		if len(keys) != 1 {
			t.Fatalf("want 1 key, got %d", len(keys))
		}
		k := keys[0]
		if k.Name != "txco" {
			t.Errorf("name = %q, want txco", k.Name)
		}
		if k.Registry != "registry.thanks.computer" {
			t.Errorf("registry = %q, want registry.thanks.computer", k.Registry)
		}
		if k.KeyID != sign.KeyIDForPub(k.Pub) || k.KeyID == "" {
			t.Errorf("keyid not derived from pub: %q", k.KeyID)
		}
	})

	t.Run("malformed entry skipped, good one kept", func(t *testing.T) {
		doc := `{"keys":[{"name":"bad","pubkey":"not-a-key"},{"name":"txco","pubkey":"` + testSigningPubkey + `"}]}`
		keys, err := parseSigningKeys([]byte(doc), "r.example")
		if err != nil {
			t.Fatalf("parseSigningKeys: %v", err)
		}
		if len(keys) != 1 || keys[0].Name != "txco" {
			t.Fatalf("want only the good key, got %+v", keys)
		}
	})

	t.Run("malformed document is an error", func(t *testing.T) {
		if _, err := parseSigningKeys([]byte("{not json"), "r.example"); err == nil {
			t.Fatal("want error for malformed JSON")
		}
	})

	t.Run("empty key list yields nothing", func(t *testing.T) {
		keys, err := parseSigningKeys([]byte(`{"keys":[]}`), "r.example")
		if err != nil {
			t.Fatalf("parseSigningKeys: %v", err)
		}
		if len(keys) != 0 {
			t.Fatalf("want 0 keys, got %d", len(keys))
		}
	})
}

func TestVerifiedLine(t *testing.T) {
	v := sign.Verdict{Signed: true, Trusted: true, KeyID: "SHA256:abc", Name: "txco"}

	t.Run("no color: name + bracketed fingerprint", func(t *testing.T) {
		got := verifiedLine(v, false)
		want := "✔ verified: signed by txco [SHA256:abc]"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no name: falls back to fingerprint", func(t *testing.T) {
		got := verifiedLine(sign.Verdict{KeyID: "SHA256:abc"}, false)
		want := "✔ verified: signed by SHA256:abc"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("color: emits ANSI but keeps the text", func(t *testing.T) {
		got := verifiedLine(v, true)
		if !strings.Contains(got, "\x1b[32m") || !strings.Contains(got, "\x1b[0m") {
			t.Errorf("expected ANSI color codes, got %q", got)
		}
		if !strings.Contains(got, "txco") || !strings.Contains(got, "SHA256:abc") {
			t.Errorf("colored line missing content: %q", got)
		}
	})
}

func TestFetchSigningKeysFrom(t *testing.T) {
	t.Run("200 → keys", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != signingKeysWellKnownPath {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"keys":[{"name":"txco","pubkey":"` + testSigningPubkey + `"}]}`))
		}))
		defer srv.Close()

		keys, err := fetchSigningKeysFrom(context.Background(), srv.Client(), srv.URL+signingKeysWellKnownPath, "registry.thanks.computer")
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(keys) != 1 || keys[0].Registry != "registry.thanks.computer" {
			t.Fatalf("want 1 registry-scoped key, got %+v", keys)
		}
	})

	t.Run("404 → no keys, no error (optional endpoint)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		keys, err := fetchSigningKeysFrom(context.Background(), srv.Client(), srv.URL+signingKeysWellKnownPath, "r.example")
		if err != nil {
			t.Fatalf("want nil error for 404, got %v", err)
		}
		if keys != nil {
			t.Fatalf("want nil keys for 404, got %+v", keys)
		}
	})
}
