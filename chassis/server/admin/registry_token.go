package admin

// The package registry's token service: POST /v1/tenants/{t}/registry/token.
//
// An OCI registry running token auth answers a request it will not serve
// with a challenge naming the scope it wants, and accepts a short-lived
// signed token that grants it (chassis/ocitoken). This endpoint is where
// such a token comes from. The request is an ordinary signed admin request,
// so who is asking, and as which tenant, is already established; what is
// decided here is the one rule the registry cannot know:
//
//	a tenant's namespace in the registry is its slug.
//
// `acme` may be granted `repository:acme/<anything>` and nothing else. For
// the platform's own names (tenants.PlatformSlug) holding the slug is not
// enough: the tenant must ALSO be the one an operator pinned the name to,
// by ID (--registry-token-platform-namespaces). A tenant called `txco` that
// is not that tenant gets nothing, and an unpinned platform name is no
// one's.
//
// The answer is all or nothing. A request naming one scope the caller may
// not have is refused whole — a registry given a narrower token than it
// asked for answers 401, and the caller is better told why here.
//
// Disabled (404) unless the signing key, its certificate, the issuer and
// the service are all configured.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/ocitoken"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// registryTokenMaxScopes bounds one request. A push asks for one scope; a
// cross-repository blob mount for two.
const registryTokenMaxScopes = 4

// registryCertWarnWindow is how far ahead of the certificate's expiry boot
// starts warning: every token stops verifying the moment it passes.
const registryCertWarnWindow = 60 * 24 * time.Hour

// resolveRegistryToken loads the token signer from config. Anything short
// of a complete, valid configuration leaves the endpoint disabled and says
// why in the log; it never stops the chassis, which has other work.
func (c *Controller) resolveRegistryToken() {
	conf := c.pu.Conf
	issuer := strings.TrimSpace(conf.RegistryTokenIssuer)
	service := strings.TrimSpace(conf.RegistryTokenService)
	haveKey := conf.RegistryTokenKeyPath != "" || conf.RegistryTokenKeyB64 != ""
	haveCert := conf.RegistryTokenCertPath != "" || conf.RegistryTokenCertB64 != ""
	if !haveKey && !haveCert && issuer == "" && service == "" {
		return // not configured: the open-core default
	}
	disabled := func(why string) {
		c.pu.Logger.Error("registry-token: disabled", zap.String("why", why))
	}
	if !haveKey || !haveCert || issuer == "" || service == "" {
		disabled("needs all of --registry-token-key, --registry-token-cert, --registry-token-issuer and --registry-token-service")
		return
	}
	keyPEM, err := registryTokenPEM(conf.RegistryTokenKeyPath, conf.RegistryTokenKeyB64)
	if err != nil {
		disabled("key: " + err.Error())
		return
	}
	certPEM, err := registryTokenPEM(conf.RegistryTokenCertPath, conf.RegistryTokenCertB64)
	if err != nil {
		disabled("certificate: " + err.Error())
		return
	}
	pins, err := parsePlatformNamespaces(conf.RegistryTokenPlatformNS)
	if err != nil {
		disabled(err.Error())
		return
	}
	now := time.Now()
	signer, err := ocitoken.Load(ocitoken.Config{
		KeyPEM: keyPEM, CertPEM: certPEM, Issuer: issuer, Service: service,
		TTL: time.Duration(conf.RegistryTokenTTL) * time.Second,
	}, now)
	if err != nil {
		disabled(err.Error())
		return
	}
	c.registrySigner, c.registryPlatformNS = signer, pins

	c.pu.Logger.Info("registry-token enabled",
		zap.String("issuer", signer.Issuer()),
		zap.String("service", signer.Service()),
		zap.Duration("ttl", signer.TTL()),
		zap.String("cert_sha256", signer.Fingerprint),
		zap.Time("cert_not_after", signer.NotAfter),
		zap.Int("platform_namespaces", len(pins)),
		// namespace → tenant id, as read: a mistyped id grants the
		// namespace to no one, and this line is where that shows.
		zap.Any("platform_pins", pins))
	if signer.NotAfter.Sub(now) < registryCertWarnWindow {
		c.pu.Logger.Warn("registry-token: the signing certificate expires soon; every token stops verifying when it does",
			zap.Time("cert_not_after", signer.NotAfter))
	}
}

// registryTokenPEM returns the PEM named by a path flag or its base64
// sibling (which wins).
func registryTokenPEM(path, b64 string) ([]byte, error) {
	if strings.TrimSpace(b64) != "" {
		return ocitoken.LoadPEM(nil, b64)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// parsePlatformNamespaces reads `namespace=tenant_id` pairs.
func parsePlatformNamespaces(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ns, id, ok := strings.Cut(p, "=")
		ns, id = strings.ToLower(strings.TrimSpace(ns)), strings.TrimSpace(id)
		if !ok || ns == "" || id == "" {
			return nil, fmt.Errorf("--registry-token-platform-namespaces: %q is not namespace=tenant_id", p)
		}
		if prev, dup := out[ns]; dup && prev != id {
			return nil, fmt.Errorf("--registry-token-platform-namespaces: %q is pinned twice", ns)
		}
		out[ns] = id
	}
	return out, nil
}

type registryTokenRequest struct {
	// Service is the registry the token is for, as the registry's
	// challenge names it. It must be the one this chassis mints for.
	Service string `json:"service"`
	// Scopes are the challenge's scopes: `repository:<name>:<actions>`.
	Scopes []string `json:"scopes"`
}

// registryTokenResponse is the Docker token-service answer (`token` and
// `access_token` are the same value under its two historical names), plus
// what was granted.
type registryTokenResponse struct {
	Token       string            `json:"token"`
	AccessToken string            `json:"access_token"`
	ExpiresIn   int               `json:"expires_in"`
	IssuedAt    string            `json:"issued_at"`
	Access      []ocitoken.Access `json:"access"`
}

// handleRegistryToken: POST /v1/tenants/{t}/registry/token
func (c *Controller) handleRegistryToken(w http.ResponseWriter, r *http.Request) {
	if c.registrySigner == nil {
		writeJSONError(w, http.StatusNotFound, "registry_token_disabled",
			map[string]any{"hint": "this chassis is not a token service for a package registry"})
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	// An operator: a super-admin, or whoever holds the chassis's own
	// credentials (basic auth, open dev) — the same line every operator-only
	// admin endpoint draws.
	operator := policy.RequireSuperAdmin(r.Context()) == nil
	// A browser session's cookie is not what publishes a package: a token
	// is a bearer credential handed to a registry client, and that client
	// is the CLI, signing with a key.
	if ac.Source == "browser" {
		writeJSONError(w, http.StatusForbidden, "source_forbidden",
			map[string]any{"hint": "registry tokens are issued to signed requests, not browser sessions"})
		return
	}

	var req registryTokenRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	// Bound to our registry: a client that follows some other registry's
	// challenge here gets no token to hand it.
	if req.Service != c.registrySigner.Service() {
		writeJSONError(w, http.StatusForbidden, "service_mismatch",
			map[string]any{"service": req.Service, "hint": "this chassis does not issue tokens for that registry"})
		return
	}
	if n := len(req.Scopes); n == 0 || n > registryTokenMaxScopes {
		writeJSONError(w, http.StatusBadRequest, "scope_invalid",
			map[string]any{"hint": fmt.Sprintf("give 1 to %d scopes", registryTokenMaxScopes)})
		return
	}

	var access []ocitoken.Access
	writes := false
	for _, raw := range req.Scopes {
		a, status, code, hint := c.registryGrant(r, ac, operator, raw)
		if code != "" {
			writeJSONError(w, status, code, map[string]any{"scope": raw, "hint": hint})
			return
		}
		for _, act := range a.Actions {
			if act != "pull" {
				writes = true
			}
		}
		access = append(access, a)
	}

	// A tenant that is suspended or disabled publishes nothing. An operator
	// acting for it is not held to that.
	if writes && !operator {
		rr, err := c.loadRuntimeRow(r.Context(), ac.TenantID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "runtime_state_lookup", map[string]any{"err": err.Error()})
			return
		}
		if rr.Enabled != 1 || rr.Suspended != 0 {
			writeJSONError(w, http.StatusForbidden, "tenant_suspended",
				map[string]any{"hint": "a suspended tenant cannot publish packages"})
			return
		}
	}

	actor := ac.ActorID
	if actor == "" {
		actor = ac.Source // basic-auth / open-dev operator: no actor row
	}
	tok, err := c.registrySigner.Mint(ac.TenantSlug+"/"+actor, ac.TenantID, access, time.Now())
	if err != nil {
		// Notably: the signing certificate has expired under a running chassis.
		c.pu.Logger.Error("registry-token: mint failed", zap.String("err", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "registry_token_sign", map[string]any{"err": err.Error()})
		return
	}
	// The audit line: who was handed what. The token itself is never logged.
	c.pu.Logger.Info("registry-token minted",
		zap.String("tenant_id", ac.TenantID),
		zap.String("tenant_slug", ac.TenantSlug),
		zap.String("actor_id", ac.ActorID),
		zap.String("key_id", ac.KeyID),
		zap.String("source", ac.Source),
		zap.Any("access", access),
		zap.String("jti", tok.ID),
		zap.Time("expires", tok.Expires))

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, registryTokenResponse{
		Token:       tok.Raw,
		AccessToken: tok.Raw,
		ExpiresIn:   int(tok.Expires.Sub(tok.IssuedAt) / time.Second),
		IssuedAt:    tok.IssuedAt.Format(time.RFC3339),
		Access:      access,
	})
}

// registryGrant decides one requested scope. It returns the access to
// grant, or a refusal as (status, code, hint) with a non-empty code.
func (c *Controller) registryGrant(r *http.Request, ac *auth.Context, operator bool, raw string) (ocitoken.Access, int, string, string) {
	refuse := func(status int, code, hint string) (ocitoken.Access, int, string, string) {
		return ocitoken.Access{}, status, code, hint
	}
	a, err := ocitoken.ParseScope(raw)
	if err != nil {
		return refuse(http.StatusBadRequest, "scope_invalid", err.Error())
	}

	// The registry's catalogue of every repository: the operator's only.
	if a.Type == "registry" {
		if a.Name != "catalog" || len(a.Actions) != 1 || a.Actions[0] != "*" {
			return refuse(http.StatusBadRequest, "scope_invalid", "the only registry scope is registry:catalog:*")
		}
		if !operator {
			return refuse(http.StatusForbidden, "capability_missing", "listing the registry's catalogue is an operator's")
		}
		return a, 0, "", ""
	}
	if a.Type != "repository" {
		return refuse(http.StatusBadRequest, "scope_invalid", "scope type must be repository")
	}

	push, del := false, false
	for _, act := range a.Actions {
		switch act {
		case "pull":
		case "push":
			push = true
		case "delete":
			del = true
		default:
			return refuse(http.StatusBadRequest, "scope_invalid", fmt.Sprintf("unknown action %q (pull, push)", act))
		}
	}

	if err := ocitoken.ValidateRepository(a.Name); err != nil {
		return refuse(http.StatusBadRequest, "namespace_invalid", err.Error())
	}
	ns, name := ocitoken.Namespace(a.Name)
	if ns != ac.TenantSlug {
		return refuse(http.StatusForbidden, "namespace_forbidden",
			fmt.Sprintf("tenant %q publishes under %s/, not %s/", ac.TenantSlug, ac.TenantSlug, ns))
	}
	// The platform's names belong to the tenant an operator pinned, by ID.
	// The slug alone proves nothing: it is checked for everyone, operators
	// included, so a `txco` tenant that is not THE txco tenant gets none.
	if pin, pinned := c.registryPlatformNS[ns]; pinned || tenants.PlatformSlug(ns) {
		if !pinned || pin != ac.TenantID {
			return refuse(http.StatusForbidden, "namespace_forbidden",
				fmt.Sprintf("the namespace %s/ is the platform's", ns))
		}
	}

	if del {
		// Removing a tag or a blob breaks whoever installed it.
		if !operator {
			return refuse(http.StatusForbidden, "capability_missing", "deleting from the registry is an operator's")
		}
	}
	if push {
		if policy.RequireCapability(r.Context(), "package:"+name+":push") != nil {
			return refuse(http.StatusForbidden, "capability_missing",
				fmt.Sprintf("publishing %s needs package:%s:push on tenant %q", a.Name, name, ac.TenantSlug))
		}
	} else if policy.RequireCapability(r.Context(), "package:"+name+":pull") != nil &&
		policy.RequireCapability(r.Context(), "package:"+name+":push") != nil {
		return refuse(http.StatusForbidden, "capability_missing",
			fmt.Sprintf("reading %s needs package:%s:pull on tenant %q", a.Name, name, ac.TenantSlug))
	}

	// A registry requires pull as well as push of whoever writes; a client
	// that asked for push alone is given both.
	out := ocitoken.Access{Type: "repository", Name: a.Name}
	out.Actions = append(out.Actions, "pull")
	if push {
		out.Actions = append(out.Actions, "push")
	}
	if del {
		out.Actions = append([]string{"delete"}, out.Actions...)
	}
	return out, 0, "", ""
}
