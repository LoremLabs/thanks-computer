package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// oidcConfig holds the subset of OIDC discovery metadata we use.
type oidcConfig struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
	JwksURI               string `json:"jwks_uri"`
	// EnrollEndpoint is a thanks-computer extension to the discovery doc: the
	// FULL chassis admin enroll URL (POST /auth/oauth/enroll). Lets the cloud
	// point onboarding at its hosted chassis without a CLI release. Empty when
	// the cloud doesn't advertise it (the CLI falls back to a constant/flag).
	EnrollEndpoint string `json:"txco_enroll_endpoint"`
}

// fallbackConfig synthesizes endpoints from the cloud base. The cloud mounts
// /auth, /token, /userinfo, /revocation under that base, so these are correct
// even when the .well-known discovery document is unreachable.
func fallbackConfig(issuer string) *oidcConfig {
	b := strings.TrimRight(issuer, "/")
	return &oidcConfig{
		Issuer:                strings.TrimRight(issuer, "/"),
		AuthorizationEndpoint: b + "/auth",
		TokenEndpoint:         b + "/token",
		UserinfoEndpoint:      b + "/userinfo",
		RevocationEndpoint:    b + "/revocation",
	}
}

// discover fetches the issuer's OpenID configuration. On any failure it
// returns fallbackConfig (ok=false) so a flaky .well-known doesn't break
// login.
func discover(ctx context.Context, hc *http.Client, issuer string) (*oidcConfig, bool) {
	u := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fallbackConfig(issuer), false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fallbackConfig(issuer), false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fallbackConfig(issuer), false
	}
	var cfg oidcConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil ||
		cfg.TokenEndpoint == "" || cfg.AuthorizationEndpoint == "" {
		return fallbackConfig(issuer), false
	}
	if cfg.UserinfoEndpoint == "" {
		cfg.UserinfoEndpoint = strings.TrimRight(issuer, "/") + "/userinfo"
	}
	if cfg.Issuer == "" {
		cfg.Issuer = strings.TrimRight(issuer, "/")
	}
	return &cfg, true
}

// genState returns a random URL-safe state token for CSRF protection.
func genState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// genPKCE returns a PKCE (verifier, S256 challenge) pair per RFC 7636.
// The verifier is 64 base64url chars (within the 43..128 bound).
func genPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 48)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// stateMismatch reports whether the returned state differs from the sent
// state, using a constant-time compare.
func stateMismatch(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1
}

// buildAuthorizeURL constructs the authorization request URL. It carries only
// the generic, CLI-originated parameters (the loopback redirect, CSRF state,
// and PKCE challenge); the cloud's /auth endpoint adds the identity-provider
// specifics (client_id, scope, prompt) before forwarding upstream.
func buildAuthorizeURL(cfg *oidcConfig, redirectURI, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return cfg.AuthorizationEndpoint + "?" + q.Encode()
}

// tokenResponse is the token endpoint's JSON.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
}

// oauthError carries the OAuth error/description from a non-2xx token
// response or a callback `error` query param.
type oauthError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *oauthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}

// exchangeCode swaps an authorization code for tokens at the cloud's token
// endpoint. The PKCE verifier proves possession; the cloud adds the
// identity-provider client_id before forwarding upstream. The redirect_uri
// must byte-match the one used at authorize time.
func exchangeCode(ctx context.Context, hc *http.Client, cfg *oidcConfig, code, verifier, redirectURI string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var oe oauthError
		if json.Unmarshal(body, &oe) == nil && oe.Code != "" {
			return nil, &oe
		}
		// Not an OAuth error body — most likely an edge/infra block (CDN or
		// firewall). Surface a snippet so the source is identifiable.
		snippet := strings.Join(strings.Fields(string(body)), " ")
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		if snippet != "" {
			return nil, fmt.Errorf("token endpoint returned %s: %s", resp.Status, snippet)
		}
		return nil, fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	return &tr, nil
}

// verifyIDToken fetches the cloud-advertised JWKS and verifies the id_token's
// signature against it (matching the token's kid), also validating temporal
// claims (exp/nbf/iat). The keys mirror the upstream identity provider that
// actually signed the token. Issuer/audience are intentionally NOT asserted:
// the token's `iss` is the upstream IdP (which the cloud brokers), not the
// cloud, and the CLI doesn't carry the client_id. Returns sub/email on success.
func verifyIDToken(ctx context.Context, hc *http.Client, jwksURI, idToken string) (sub, email string, err error) {
	set, err := jwk.Fetch(ctx, jwksURI, jwk.WithHTTPClient(hc))
	if err != nil {
		return "", "", fmt.Errorf("fetch jwks: %w", err)
	}
	tok, err := jwt.Parse([]byte(idToken), jwt.WithKeySet(set))
	if err != nil {
		return "", "", fmt.Errorf("verify id_token: %w", err)
	}
	if v, ok := tok.Get("email"); ok {
		if s, ok2 := v.(string); ok2 {
			email = s
		}
	}
	return tok.Subject(), email, nil
}

// claimsFromIDToken does an UNVERIFIED decode of a JWT's payload to read
// sub/email. Fallback used only when the cloud advertises no jwks_uri (e.g.
// discovery is unreachable). The id_token was just received directly from the
// cloud's token endpoint over TLS in the same exchange, so it isn't
// attacker-controlled here. Never use this for a trust decision.
func claimsFromIDToken(idToken string) (sub, email string) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Sub, claims.Email
}

// revokeToken best-effort revokes a token at the cloud (RFC 7009). The cloud
// adds the identity-provider client_id before forwarding upstream.
func revokeToken(ctx context.Context, hc *http.Client, cfg *oidcConfig, token string) error {
	if cfg.RevocationEndpoint == "" {
		return fmt.Errorf("cloud has no revocation endpoint")
	}
	form := url.Values{}
	form.Set("token", token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.RevocationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("revocation returned %s", resp.Status)
	}
	return nil
}

// userAgent identifies the CLI to the cloud (rather than Go's default
// "Go-http-client/…").
const userAgent = "txco-cli"

// cliTransport stamps a User-Agent and a same-origin Origin on outbound
// requests. The cloud is a SvelteKit app, and SvelteKit's built-in CSRF
// protection rejects cross-origin form POSTs (the /token exchange) with
// "Cross-site POST form submissions are forbidden". Setting Origin to the
// target's own origin makes our trusted API call register as same-origin and
// pass that check.
type cliTransport struct{ base http.RoundTripper }

func (t cliTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	needsUA := r.Header.Get("User-Agent") == ""
	needsOrigin := r.Header.Get("Origin") == ""
	if needsUA || needsOrigin {
		r = r.Clone(r.Context())
		if needsUA {
			r.Header.Set("User-Agent", userAgent)
		}
		if needsOrigin {
			r.Header.Set("Origin", r.URL.Scheme+"://"+r.URL.Host)
		}
	}
	return t.base.RoundTrip(r)
}

// newHTTPClient builds the HTTP client for OIDC calls. insecure disables TLS
// verification and is honored ONLY for a local dev cloud (the caller guards
// this against non-loopback hosts).
func newHTTPClient(insecure bool) *http.Client {
	var base http.RoundTripper = http.DefaultTransport
	if insecure {
		base = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev-only, gated to a loopback host in login.go
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: cliTransport{base: base}}
}
