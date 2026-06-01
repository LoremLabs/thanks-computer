package cloud

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenPKCE(t *testing.T) {
	verifier, challenge, err := genPKCE()
	if err != nil {
		t.Fatalf("genPKCE: %v", err)
	}
	if n := len(verifier); n < 43 || n > 128 {
		t.Errorf("verifier length %d outside RFC 7636 bound 43..128", n)
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if challenge != want {
		t.Errorf("challenge != S256(verifier)\n got %q\nwant %q", challenge, want)
	}

	// Two calls must differ (randomness).
	v2, _, _ := genPKCE()
	if v2 == verifier {
		t.Error("two genPKCE verifiers were identical")
	}
}

func TestStateMismatch(t *testing.T) {
	if stateMismatch("abc", "abc") {
		t.Error("equal states reported as mismatch")
	}
	if !stateMismatch("abc", "abd") {
		t.Error("different states reported as match")
	}
	if !stateMismatch("abc", "abcd") {
		t.Error("different-length states reported as match")
	}
	if !stateMismatch("", "abc") {
		t.Error("empty got vs non-empty want reported as match")
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	cfg := fallbackConfig("https://www.thanks.computer")
	raw := buildAuthorizeURL(cfg, "http://127.0.0.1:45455/callback", "st4te", "ch4llenge")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://www.thanks.computer/auth" {
		t.Errorf("authorize endpoint = %q, want https://www.thanks.computer/auth", got)
	}
	q := u.Query()
	checks := map[string]string{
		"redirect_uri":          "http://127.0.0.1:45455/callback",
		"response_type":         "code",
		"state":                 "st4te",
		"code_challenge":        "ch4llenge",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("query %q = %q, want %q", k, got, want)
		}
	}
	// The CLI must NOT carry identity-provider specifics — the cloud's /auth
	// endpoint adds them before forwarding upstream.
	for _, k := range []string{"client_id", "scope"} {
		if got := q.Get(k); got != "" {
			t.Errorf("authorize URL should not contain %q (the cloud adds it), got %q", k, got)
		}
	}
}

func TestTokenStoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TXCO_HOME", home)

	now := time.Unix(1_900_000_000, 0).UTC()
	in := CloudToken{
		Kind:         "cloud",
		AccessToken:  "at-123",
		RefreshToken: "rt-456",
		IDToken:      "id.tok.en",
		TokenType:    "Bearer",
		Scope:        "openid profile email offline_access",
		Expiry:       now.Add(time.Hour),
		ObtainedAt:   now,
		Subject:      "email:matt@example.com",
		Email:        "matt@example.com",
		Issuer:       defaultCloudURL,
		ClientID:     "",
		CloudURL:     defaultCloudURL,
	}
	if err := SaveCloudToken("cloud", in); err != nil {
		t.Fatalf("SaveCloudToken: %v", err)
	}

	// File exists at the expected path with 0600 perms.
	path := filepath.Join(home, "cloud", "cloud.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 600", perm)
	}

	// No leftover temp files in the cloud dir.
	entries, _ := os.ReadDir(filepath.Join(home, "cloud"))
	if len(entries) != 1 {
		t.Errorf("cloud dir has %d entries, want 1 (temp file not cleaned up?)", len(entries))
	}

	out, err := LoadCloudToken("cloud")
	if err != nil {
		t.Fatalf("LoadCloudToken: %v", err)
	}
	if out.AccessToken != in.AccessToken || out.RefreshToken != in.RefreshToken ||
		out.Subject != in.Subject || out.Email != in.Email || out.Issuer != in.Issuer ||
		out.ClientID != in.ClientID || out.CloudURL != in.CloudURL || out.Scope != in.Scope {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
	if !out.Expiry.Equal(in.Expiry) || !out.ObtainedAt.Equal(in.ObtainedAt) {
		t.Errorf("time round-trip mismatch: expiry %v/%v, obtained %v/%v",
			out.Expiry, in.Expiry, out.ObtainedAt, in.ObtainedAt)
	}
}

func TestLoadMissingTokenIsNotExist(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	if _, err := LoadCloudToken("nope"); err == nil {
		t.Error("expected error loading a missing token")
	}
	existed, err := DeleteCloudToken("nope")
	if err != nil {
		t.Errorf("DeleteCloudToken on missing file errored: %v", err)
	}
	if existed {
		t.Error("DeleteCloudToken reported existed=true for a missing file")
	}
}

func TestExpired(t *testing.T) {
	now := time.Unix(1_900_000_000, 0).UTC()
	if (&CloudToken{}).Expired(now) {
		t.Error("zero-expiry token reported expired")
	}
	if (&CloudToken{Expiry: now.Add(time.Hour)}).Expired(now) {
		t.Error("future-expiry token reported expired")
	}
	if !(&CloudToken{Expiry: now.Add(-time.Hour)}).Expired(now) {
		t.Error("past-expiry token reported not expired")
	}
	// Within the 30s negative skew → treated as expired.
	if !(&CloudToken{Expiry: now.Add(10 * time.Second)}).Expired(now) {
		t.Error("token inside skew window not reported expired")
	}
}

func TestValidProfileName(t *testing.T) {
	for _, ok := range []string{"cloud", "local", "prod", "matt-dev"} {
		if !validProfileName(ok) {
			t.Errorf("validProfileName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../escape"} {
		if validProfileName(bad) {
			t.Errorf("validProfileName(%q) = true, want false", bad)
		}
	}
}
