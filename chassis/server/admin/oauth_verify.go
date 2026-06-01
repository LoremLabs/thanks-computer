package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"go.uber.org/zap"
)

// resolveOAuthIssuer wires up id_token verification for
// POST /auth/oauth/enroll. It is a no-op — the endpoint stays disabled —
// when --cloud-oauth-issuer is empty. Open-core trusts no external issuer
// by default; the hosted product seeds the canonical issuer at the
// service layer (cmd/txco-saas/main.go).
//
// JWKS resolution and the first key fetch are LAZY: Start must not block
// or fail because the IdP is briefly unreachable at boot. We resolve the
// jwks_uri from the issuer's discovery doc once (falling back to the
// oidc-provider conventional /jwks), register it with an auto-refreshing
// cache, and let the first verify fetch the keys.
func (c *Controller) resolveOAuthIssuer() {
	issuer := strings.TrimRight(strings.TrimSpace(c.pu.Conf.CloudOAuthIssuer), "/")
	if issuer == "" {
		return
	}
	c.oauthIssuer = issuer
	c.oauthAudience = strings.TrimSpace(c.pu.Conf.CloudOAuthAudience)

	jwksURI := resolveJWKSURI(c.ctx, issuer)
	cache := jwk.NewCache(c.ctx)
	if err := cache.Register(jwksURI, jwk.WithMinRefreshInterval(15*time.Minute)); err != nil {
		// Non-fatal: leave the endpoint enabled and let per-request
		// verification surface a clear error until the key source is
		// reachable. Boot must not depend on the IdP being up this instant.
		c.pu.Logger.Warn("oauth-enroll: jwks register failed",
			zap.String("jwks_uri", jwksURI), zap.String("err", err.Error()))
	}
	c.oauthJWKS = jwk.NewCachedSet(cache, jwksURI)

	c.pu.Logger.Info("oauth-enroll enabled",
		zap.String("issuer", issuer),
		zap.String("jwks_uri", jwksURI),
		zap.Bool("audience_checked", c.oauthAudience != ""))
}

// resolveJWKSURI reads the issuer's discovery doc for jwks_uri, falling
// back to <issuer>/jwks (oidc-provider serves keys there and rejects
// /.well-known/jwks.json). Mirrors the CLI's resolveJwksUri.
func resolveJWKSURI(ctx context.Context, issuer string) string {
	fallback := issuer + "/jwks"
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return fallback
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fallback
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fallback
	}
	var cfg struct {
		JwksURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil || cfg.JwksURI == "" {
		return fallback
	}
	return cfg.JwksURI
}

// verifyOAuthIDToken verifies the id_token's RS256 signature against the
// configured issuer's JWKS and validates exp/nbf/iss (and aud when an
// audience is configured). Returns the subject claim on success.
//
// The raw id_token is a bearer secret: it is never logged here, and
// callers must not log it either.
func (c *Controller) verifyOAuthIDToken(ctx context.Context, raw string) (string, error) {
	if c.oauthJWKS == nil {
		return "", fmt.Errorf("oauth issuer not configured")
	}
	opts := []jwt.ParseOption{
		jwt.WithContext(ctx),
		jwt.WithKeySet(c.oauthJWKS),
		jwt.WithValidate(true),
		jwt.WithIssuer(c.oauthIssuer),
	}
	if c.oauthAudience != "" {
		opts = append(opts, jwt.WithAudience(c.oauthAudience))
	}
	tok, err := jwt.Parse([]byte(raw), opts...)
	if err != nil {
		return "", err
	}
	sub := tok.Subject()
	if sub == "" {
		return "", fmt.Errorf("id_token missing sub")
	}
	return sub, nil
}
