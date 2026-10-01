package cli

// Registry tokens: how `txco package publish` is allowed to write.
//
// A registry that scopes writes per tenant runs token auth: it answers a
// push with a Bearer challenge and accepts a short-lived token naming the
// exact repository. For such a registry the token service is a chassis —
// POST /v1/tenants/{t}/registry/token (chassis/server/admin/registry_token.go)
// — and the credential is the signed-in profile, so there is no registry
// password to hold: whoever may act for tenant `acme` may publish under
// `acme/`.
//
// The registry says which chassis at a well-known path; the tenant is the
// FIRST path segment of the package being pushed; the request is signed by
// a profile enrolled at that chassis. All of it happens only after the
// registry refuses a request, so reading a public package asks no one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

func init() { source.SetCredentialProvider(registryTokenProvider) }

// registryWellKnownPath is where a registry names the chassis that issues
// its tokens: {"token_service": "https://admin.example.com"}.
const registryWellKnownPath = "/.well-known/txco-registry.json"

// registryTokenTimeout bounds the token request.
const registryTokenTimeout = 20 * time.Second

// registryTokenProfile is `txco package publish --profile`: the profile
// that signs the token request, when the caller says. Empty leaves the
// choice to pickRegistryProfile.
var registryTokenProfile string

type registryDoc struct {
	TokenService string `json:"token_service"`
}

// registryBaseURL is the URL a registry host is reached at: https, or
// plain http for a registry on this machine.
func registryBaseURL(host string) string {
	if source.LoopbackHost(host) {
		return "http://" + host
	}
	return "https://" + host
}

// tokenServices caches discovery per registry host for the life of the
// process ("" = the registry names none). Only an ANSWER is cached; a
// registry that could not be asked is asked again. Tokens are never cached.
var tokenServices sync.Map

// registryTokenService returns the chassis that issues tokens for a
// registry, or "" when the registry names none: the caller then falls back
// to docker credentials, which is what a registry with its own token realm
// wants.
func registryTokenService(ctx context.Context, hostport string) (string, error) {
	if v, ok := tokenServices.Load(hostport); ok {
		return v.(string), nil
	}
	svc, err := fetchRegistryTokenService(ctx, http.DefaultClient,
		registryBaseURL(hostport)+registryWellKnownPath, source.LoopbackHost(hostport))
	if err != nil {
		return "", fmt.Errorf("%s: %v", hostport, err)
	}
	tokenServices.Store(hostport, svc)
	return svc, nil
}

// fetchRegistryTokenService is the testable core of discovery.
//
// "Names none" is ("", nil) and is something the registry SAID: any 4xx, or
// a 200 that is not the document. A registry that could not be asked — a
// transport failure, a 5xx — is an error, and so is a document naming a
// service this CLI must not send a request to. Neither may be read as
// "none": that would hand a challenge meant for the chassis to whatever
// docker password is on file for the host.
//
// registryIsLocal says the registry is on this machine; only then may it
// name a chassis on this machine over plain http.
func fetchRegistryTokenService(ctx context.Context, hc *http.Client, docURL string, registryIsLocal bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, wellKnownTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not ask the registry who issues its tokens: %v", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", nil
	default:
		return "", fmt.Errorf("could not ask the registry who issues its tokens: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWellKnownBytes))
	if err != nil {
		return "", fmt.Errorf("could not ask the registry who issues its tokens: %v", err)
	}
	var doc registryDoc
	if json.Unmarshal(body, &doc) != nil || strings.TrimSpace(doc.TokenService) == "" {
		return "", nil
	}
	origin, err := chassisOrigin(doc.TokenService)
	if err != nil {
		return "", fmt.Errorf("the registry names a token service this CLI will not use: %v", err)
	}
	if strings.HasPrefix(origin, "http://") && !registryIsLocal {
		return "", errors.New("the registry names a token service this CLI will not use: it is not https")
	}
	return origin, nil
}

// chassisOrigin normalises a chassis URL to scheme://host[:port]. It
// accepts https, and plain http only to this machine (a dev chassis), where
// every loopback spelling — localhost, 127.x.y.z, ::1 — is written
// `localhost`, so two URLs for one local chassis compare equal.
func chassisOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return "", errors.New("not a URL")
	}
	if u.User != nil {
		return "", errors.New("it carries credentials")
	}
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	local := source.LoopbackHost(host)
	switch {
	case scheme == "https":
		if port == "443" {
			port = ""
		}
	case scheme == "http" && local:
		if port == "80" {
			port = ""
		}
	default:
		return "", errors.New("it is not https")
	}
	if local {
		host = "localhost"
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

// sameOrigin compares a profile's recorded chassis_url with a token
// service origin (already normalised by chassisOrigin).
func sameOrigin(chassisURL, origin string) bool {
	got, err := chassisOrigin(chassisURL)
	return err == nil && got == origin
}

// registryTokenProvider is the source.CredentialProvider: asked for a
// token when a registry answers a request with a Bearer challenge.
func registryTokenProvider(ctx context.Context, hostport string, scopes []string) (string, bool, error) {
	var repoScopes []string
	for _, s := range scopes {
		if strings.HasPrefix(s, "repository:") {
			repoScopes = append(repoScopes, s)
		}
	}
	if len(repoScopes) == 0 {
		return "", false, nil
	}
	svc, err := registryTokenService(ctx, hostport)
	if err != nil {
		return "", true, err
	}
	if svc == "" {
		return "", false, nil
	}
	tenant, err := scopesTenant(repoScopes)
	if err != nil {
		return "", true, err
	}

	profile, sg, err := pickRegistryProfile(svc, tenant, hostport)
	if err != nil {
		return "", true, err
	}

	ctx, cancel := context.WithTimeout(ctx, registryTokenTimeout)
	defer cancel()
	var out struct {
		Token string `json:"token"`
	}
	c := client.New(client.Target{Addr: svc, Tenant: tenant, Auth: sg})
	err = c.DoScoped(ctx, http.MethodPost, "/registry/token",
		map[string]any{"service": hostport, "scopes": repoScopes}, &out)
	if err != nil {
		handled, err := registryTokenError(err, hostport, tenant, profile)
		return "", handled, err
	}
	if out.Token == "" {
		return "", true, fmt.Errorf("the chassis for %s answered without a token", hostport)
	}
	return out.Token, true, nil
}

// scopesTenant is the tenant a set of repository scopes belongs to: the
// first path segment of each name. One request is for one tenant.
func scopesTenant(scopes []string) (string, error) {
	tenant := ""
	for _, s := range scopes {
		// repository:<name>:<actions>
		name := strings.TrimPrefix(s, "repository:")
		if i := strings.LastIndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		ns, rest, ok := strings.Cut(name, "/")
		if !ok || ns == "" || rest == "" {
			return "", fmt.Errorf("package %q has no namespace: publish as <tenant>/<name>, where <tenant> is your tenant", name)
		}
		if tenant != "" && ns != tenant {
			return "", fmt.Errorf("one publish spans two namespaces (%s/ and %s/)", tenant, ns)
		}
		tenant = ns
	}
	return tenant, nil
}

// pickRegistryProfile chooses the profile that signs the token request,
// among those enrolled at the token service's chassis:
//
//  1. the one the caller named (--profile, TXCO_PROFILE, or the key at
//     TXCO_PRIVATE_KEY_PATH) — it must be enrolled there;
//  2. else one whose tenant is the namespace being published to;
//  3. else the workspace's / active profile, if it is enrolled there;
//  4. else the only one that is.
//
// A chassis on this machine with no profile is asked unsigned (`txco dev`
// runs open). The returned name is for messages; the signer may be nil only
// in that last case.
func pickRegistryProfile(svc, tenant, registryHost string) (string, signer.Signer, error) {
	if explicit := os.Getenv("TXCO_PRIVATE_KEY_PATH"); explicit != "" {
		metaPath := explicit + ".meta.json"
		m, err := auth.LoadMeta(metaPath)
		if err != nil {
			return "", nil, fmt.Errorf("TXCO_PRIVATE_KEY_PATH: %v", err)
		}
		if !sameOrigin(m.ChassisURL, svc) {
			return "", nil, fmt.Errorf("the key at TXCO_PRIVATE_KEY_PATH is not enrolled at the chassis that issues tokens for %s", registryHost)
		}
		sg, err := auth.LoadSignerForMetaPath(metaPath)
		if err != nil {
			return "", nil, err
		}
		return explicit, sg, nil
	}

	profiles, err := auth.ListProfiles()
	if err != nil {
		return "", nil, err
	}
	var enrolled []auth.ProfileInfo
	for _, p := range profiles {
		if p.Meta != nil && sameOrigin(p.Meta.ChassisURL, svc) {
			enrolled = append(enrolled, p)
		}
	}
	find := func(name string) *auth.ProfileInfo {
		for i := range enrolled {
			if enrolled[i].Name == name {
				return &enrolled[i]
			}
		}
		return nil
	}
	use := func(name string) (string, signer.Signer, error) {
		sg, err := auth.LoadSignerForName(name)
		if err != nil {
			return "", nil, fmt.Errorf("profile %q: %v", name, err)
		}
		if sg == nil {
			return "", nil, fmt.Errorf("profile %q has no usable signing key (txco login)", name)
		}
		return name, sg, nil
	}

	named := registryTokenProfile
	if named == "" {
		named = os.Getenv("TXCO_PROFILE")
	}
	if named != "" {
		if find(named) == nil {
			return "", nil, fmt.Errorf("profile %q is not enrolled at the chassis that issues tokens for %s", named, registryHost)
		}
		return use(named)
	}

	// ListProfiles sorts the active profile first, so of several profiles
	// for the tenant the active one wins.
	for _, p := range enrolled {
		if p.Meta.DefaultTenant == tenant {
			return use(p.Name)
		}
	}
	if resolved, err := auth.ResolveProfile(""); err == nil && resolved != auth.ActiveNone && find(resolved) != nil {
		return use(resolved)
	}
	switch len(enrolled) {
	case 1:
		return use(enrolled[0].Name)
	case 0:
		if u, err := url.Parse(svc); err == nil && source.LoopbackHost(u.Host) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("publishing to %s needs a txco profile signed in to its chassis, and none of yours is (txco auth profiles)", registryHost)
	default:
		names := make([]string, len(enrolled))
		for i, p := range enrolled {
			names[i] = p.Name
		}
		sort.Strings(names)
		return "", nil, fmt.Errorf("several profiles could publish to %s (%s) and none is for tenant %q: pass --profile", registryHost, strings.Join(names, ", "), tenant)
	}
}

// registryTokenError turns the chassis's refusal into what the publisher
// should read. handled=false hands the challenge to docker credentials:
// the chassis is not a token service after all.
func registryTokenError(err error, registryHost, tenant, profile string) (bool, error) {
	var he *client.HTTPError
	if !errors.As(err, &he) {
		return true, fmt.Errorf("registry token for %s: %w", registryHost, err)
	}
	as := "unsigned"
	if profile != "" {
		as = fmt.Sprintf("profile %q", profile)
	}
	hint, _ := he.Detail["hint"].(string)
	withHint := func(msg string) error {
		if hint != "" {
			return fmt.Errorf("%s: %s", msg, hint)
		}
		return errors.New(msg)
	}
	switch {
	case he.StatusCode == http.StatusNotFound && he.Code == "tenant_not_found":
		return true, fmt.Errorf("no tenant %q: a package is published as <tenant>/<name>, and %s/ is not a tenant of %s's chassis", tenant, tenant, registryHost)
	case he.StatusCode == http.StatusNotFound:
		// registry_token_disabled, or a chassis that predates the endpoint.
		return false, nil
	case he.StatusCode == http.StatusUnauthorized:
		return true, fmt.Errorf("the chassis for %s did not accept the request signed as %s (txco login)", registryHost, as)
	case he.Code == "capability_missing":
		return true, withHint(fmt.Sprintf("%s may not publish under %s/", as, tenant))
	case he.Code == "namespace_forbidden", he.Code == "tenant_suspended", he.Code == "service_mismatch",
		he.Code == "source_forbidden", he.Code == "namespace_invalid", he.Code == "scope_invalid":
		return true, withHint(fmt.Sprintf("registry token refused (%s)", he.Code))
	}
	return true, fmt.Errorf("registry token for %s: %w", registryHost, err)
}
