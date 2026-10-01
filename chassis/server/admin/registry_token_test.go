package admin

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jws"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/ocitoken"
)

const testRegistryService = "registry.test"

// registryTokenPair is a P-256 key and its self-signed certificate, PEM.
func registryTokenPair(t *testing.T, notAfter time.Time) (keyPEM, certPEM []byte, pub *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "registry token signer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	cder, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cder}), &key.PublicKey
}

// newRegistryTokenController is a controller with the token service on,
// minting for testRegistryService, with `txco` pinned to tnt_txco.
func newRegistryTokenController(t *testing.T) (*Controller, *ecdsa.PublicKey) {
	t.Helper()
	c := newRuntimeStateTestController(t)
	keyPEM, certPEM, pub := registryTokenPair(t, time.Now().Add(365*24*time.Hour))
	signer, err := ocitoken.Load(ocitoken.Config{
		KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "https://admin.test", Service: testRegistryService,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c.registrySigner = signer
	c.registryPlatformNS = map[string]string{"txco": "tnt_txco"}
	return c, pub
}

// registryCaller is the auth.Context resolveTenantMiddleware would leave.
type registryCaller struct {
	slug, tenantID string
	caps           []string
	source         string
	superAdmin     bool
}

func (rc registryCaller) ctx() *auth.Context {
	src := rc.source
	if src == "" {
		src = "signed"
	}
	return &auth.Context{
		Source: src, ActorID: "actor_test", KeyID: "key_test",
		Capabilities: rc.caps, TenantID: rc.tenantID, TenantSlug: rc.slug, SuperAdmin: rc.superAdmin,
	}
}

var acmeOwner = registryCaller{slug: "acme", tenantID: "tnt_acme", caps: auth.TenantOwnerCaps()}

func registryTokenCall(t *testing.T, c *Controller, who registryCaller, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw = mustJSON(t, body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+who.slug+"/registry/token", bytes.NewReader(raw))
	req = req.WithContext(auth.WithContext(req.Context(), who.ctx()))
	w := httptest.NewRecorder()
	c.handleRegistryToken(w, req)
	out := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("status %d, body is not JSON: %s", w.Code, w.Body.String())
	}
	return w.Code, out
}

func scopesReq(scopes ...string) registryTokenRequest {
	return registryTokenRequest{Service: testRegistryService, Scopes: scopes}
}

// tokenClaims verifies a minted token's signature and returns its claims.
func tokenClaims(t *testing.T, raw string, pub *ecdsa.PublicKey) map[string]any {
	t.Helper()
	payload, err := jws.Verify([]byte(raw), jws.WithKey(jwa.ES256, pub))
	if err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
	var cl map[string]any
	if err := json.Unmarshal(payload, &cl); err != nil {
		t.Fatal(err)
	}
	return cl
}

func TestRegistryTokenOwnerPush(t *testing.T) {
	c, pub := newRegistryTokenController(t)
	// A client asks for what the registry's challenge named; push alone is
	// granted with the pull a registry also requires of a writer.
	for _, scope := range []string{"repository:acme/pony-web:pull,push", "repository:acme/pony-web:push"} {
		code, body := registryTokenCall(t, c, acmeOwner, scopesReq(scope))
		if code != http.StatusOK {
			t.Fatalf("%s: status %d body %v", scope, code, body)
		}
		tok, _ := body["token"].(string)
		if tok == "" || body["access_token"] != tok {
			t.Fatalf("token/access_token = %v / %v", body["token"], body["access_token"])
		}
		if body["expires_in"] != float64(300) {
			t.Fatalf("expires_in = %v, want 300", body["expires_in"])
		}
		cl := tokenClaims(t, tok, pub)
		if cl["iss"] != "https://admin.test" || cl["aud"] != testRegistryService ||
			cl["sub"] != "acme/actor_test" || cl["tenant_id"] != "tnt_acme" {
			t.Fatalf("claims = %v", cl)
		}
		want := []any{map[string]any{"type": "repository", "name": "acme/pony-web", "actions": []any{"pull", "push"}}}
		if !reflect.DeepEqual(cl["access"], want) {
			t.Fatalf("access = %v, want %v", cl["access"], want)
		}
		if !reflect.DeepEqual(body["access"], want) {
			t.Fatalf("response access = %v, want %v", body["access"], want)
		}
	}
}

func TestRegistryTokenNestedNameAndTwoScopes(t *testing.T) {
	c, pub := newRegistryTokenController(t)
	// A cross-repository blob mount: push to one, pull from another, both
	// in the tenant's namespace.
	code, body := registryTokenCall(t, c, acmeOwner,
		scopesReq("repository:acme/tools/web:pull,push", "repository:acme/base:pull"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	cl := tokenClaims(t, body["token"].(string), pub)
	want := []any{
		map[string]any{"type": "repository", "name": "acme/tools/web", "actions": []any{"pull", "push"}},
		map[string]any{"type": "repository", "name": "acme/base", "actions": []any{"pull"}},
	}
	if !reflect.DeepEqual(cl["access"], want) {
		t.Fatalf("access = %v", cl["access"])
	}
}

func TestRegistryTokenCapabilities(t *testing.T) {
	c, _ := newRegistryTokenController(t)
	member := func(caps ...string) registryCaller {
		return registryCaller{slug: "acme", tenantID: "tnt_acme", caps: caps}
	}
	for _, tc := range []struct {
		name  string
		who   registryCaller
		scope string
		code  int
		err   string
	}{
		{"push-only member pushes", member("package:*:push"), "repository:acme/x:pull,push", 200, ""},
		{"push-only member pulls", member("package:*:push"), "repository:acme/x:pull", 200, ""},
		{"pull-only member pulls", member("package:*:pull"), "repository:acme/x:pull", 200, ""},
		{"pull-only member cannot push", member("package:*:pull"), "repository:acme/x:pull,push", 403, "capability_missing"},
		{"a member scoped to one package", member("package:x:push"), "repository:acme/x:push", 200, ""},
		{"…cannot push another", member("package:x:push"), "repository:acme/y:push", 403, "capability_missing"},
		{"a nested package is its whole path", member("package:tools/web:push"), "repository:acme/tools/web:push", 200, ""},
		{"no package capability", member("opstack:*:*", "secret:*:*"), "repository:acme/x:pull", 403, "capability_missing"},
		{"no capability at all", member(), "repository:acme/x:pull,push", 403, "capability_missing"},
		{"owner cannot delete", acmeOwner, "repository:acme/x:delete", 403, "capability_missing"},
		{"owner cannot list the catalogue", acmeOwner, "registry:catalog:*", 403, "capability_missing"},
	} {
		code, body := registryTokenCall(t, c, tc.who, scopesReq(tc.scope))
		if code != tc.code || (tc.err != "" && body["error"] != tc.err) {
			t.Errorf("%s: status %d error %v, want %d %q", tc.name, code, body["error"], tc.code, tc.err)
		}
		if code != 200 && body["token"] != nil {
			t.Errorf("%s: a refusal carried a token", tc.name)
		}
	}
}

func TestRegistryTokenRefusals(t *testing.T) {
	c, _ := newRegistryTokenController(t)
	for _, tc := range []struct {
		name string
		who  registryCaller
		body any
		code int
		err  string
	}{
		{"another registry", acmeOwner, registryTokenRequest{Service: "evil.example", Scopes: []string{"repository:acme/x:push"}}, 403, "service_mismatch"},
		{"no service", acmeOwner, registryTokenRequest{Scopes: []string{"repository:acme/x:push"}}, 403, "service_mismatch"},
		{"no scopes", acmeOwner, scopesReq(), 400, "scope_invalid"},
		{"too many scopes", acmeOwner, scopesReq("repository:acme/a:pull", "repository:acme/b:pull", "repository:acme/c:pull", "repository:acme/d:pull", "repository:acme/e:pull"), 400, "scope_invalid"},
		{"malformed scope", acmeOwner, scopesReq("repository:acme/x"), 400, "scope_invalid"},
		{"not a repository", acmeOwner, scopesReq("plugin:acme/x:pull"), 400, "scope_invalid"},
		{"unknown registry scope", acmeOwner, scopesReq("registry:everything:*"), 400, "scope_invalid"},
		{"wildcard action", acmeOwner, scopesReq("repository:acme/x:*"), 400, "scope_invalid"},
		{"unknown action", acmeOwner, scopesReq("repository:acme/x:pull,admin"), 400, "scope_invalid"},
		{"no namespace", acmeOwner, scopesReq("repository:acme:push"), 400, "namespace_invalid"},
		{"not an OCI name", acmeOwner, scopesReq("repository:acme/Pony:push"), 400, "namespace_invalid"},
		{"a wildcard name", acmeOwner, scopesReq("repository:acme/*:push"), 400, "namespace_invalid"},
		{"a name on the upload route", acmeOwner, scopesReq("repository:acme/blobs/uploads/x:push"), 400, "namespace_invalid"},
		{"another tenant's namespace", acmeOwner, scopesReq("repository:onepony/x:push"), 403, "namespace_forbidden"},
		{"another tenant's, to read", acmeOwner, scopesReq("repository:onepony/x:pull"), 403, "namespace_forbidden"},
		{"a prefix of the tenant's name", acmeOwner, scopesReq("repository:acm/x:push"), 403, "namespace_forbidden"},
		{"the tenant's name as a prefix", acmeOwner, scopesReq("repository:acme-evil/x:push"), 403, "namespace_forbidden"},
		{"the platform's namespace", acmeOwner, scopesReq("repository:txco/hello-world:push"), 403, "namespace_forbidden"},
		{"one bad scope refuses all", acmeOwner, scopesReq("repository:acme/x:push", "repository:txco/x:push"), 403, "namespace_forbidden"},
		{"browser session", registryCaller{slug: "acme", tenantID: "tnt_acme", caps: []string{"admin:all"}, source: "browser"}, scopesReq("repository:acme/x:push"), 403, "source_forbidden"},
		{"unknown field", acmeOwner, `{"service":"registry.test","scopes":["repository:acme/x:push"],"ttl":86400}`, 400, "invalid JSON body"},
		{"not JSON", acmeOwner, `nope`, 400, "invalid JSON body"},
		// A slug that is not an OCI path component has no namespace to publish under.
		{"slug not an OCI component", registryCaller{slug: "a.b_", tenantID: "tnt_odd", caps: auth.TenantOwnerCaps()}, scopesReq("repository:a.b_/x:push"), 400, "namespace_invalid"},
	} {
		code, body := registryTokenCall(t, c, tc.who, tc.body)
		if code != tc.code || body["error"] != tc.err {
			t.Errorf("%s: status %d error %v, want %d %q", tc.name, code, body["error"], tc.code, tc.err)
		}
		if body["token"] != nil {
			t.Errorf("%s: a refusal carried a token", tc.name)
		}
	}
}

// The platform's namespace follows the tenant ID an operator pinned, never
// the slug: a tenant that merely holds the name gets nothing.
func TestRegistryTokenPlatformNamespace(t *testing.T) {
	c, pub := newRegistryTokenController(t)
	scope := scopesReq("repository:txco/hello-world:pull,push")

	pinned := registryCaller{slug: "txco", tenantID: "tnt_txco", caps: auth.TenantOwnerCaps()}
	code, body := registryTokenCall(t, c, pinned, scope)
	if code != http.StatusOK {
		t.Fatalf("pinned tenant: status %d body %v", code, body)
	}
	if cl := tokenClaims(t, body["token"].(string), pub); cl["tenant_id"] != "tnt_txco" {
		t.Fatalf("claims = %v", cl)
	}

	impostor := registryCaller{slug: "txco", tenantID: "tnt_someone_else", caps: auth.TenantOwnerCaps()}
	if code, body := registryTokenCall(t, c, impostor, scope); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
		t.Fatalf("slug without the pinned id: status %d error %v", code, body["error"])
	}
	// An operator is held to the pin too.
	super := registryCaller{slug: "txco", tenantID: "tnt_someone_else", superAdmin: true}
	if code, body := registryTokenCall(t, c, super, scope); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
		t.Fatalf("super-admin on an unpinned txco: status %d error %v", code, body["error"])
	}
	// A platform name nobody pinned is nobody's.
	library := registryCaller{slug: "library", tenantID: "tnt_lib", caps: auth.TenantOwnerCaps()}
	if code, body := registryTokenCall(t, c, library, scopesReq("repository:library/x:push")); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
		t.Fatalf("unpinned platform name: status %d error %v", code, body["error"])
	}
	// An operator may pin a name that is not on the built-in list.
	c.registryPlatformNS["acme"] = "tnt_the_real_acme"
	if code, body := registryTokenCall(t, c, acmeOwner, scopesReq("repository:acme/x:push")); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
		t.Fatalf("pinned ordinary name, wrong tenant: status %d error %v", code, body["error"])
	}
}

func TestRegistryTokenSuperAdmin(t *testing.T) {
	c, pub := newRegistryTokenController(t)
	super := registryCaller{slug: "acme", tenantID: "tnt_acme", superAdmin: true}

	code, body := registryTokenCall(t, c, super, scopesReq("repository:acme/x:delete", "registry:catalog:*"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	cl := tokenClaims(t, body["token"].(string), pub)
	want := []any{
		map[string]any{"type": "repository", "name": "acme/x", "actions": []any{"delete", "pull"}},
		map[string]any{"type": "registry", "name": "catalog", "actions": []any{"*"}},
	}
	if !reflect.DeepEqual(cl["access"], want) {
		t.Fatalf("access = %v, want %v", cl["access"], want)
	}
	// Even an operator names the tenant whose namespace it is.
	if code, body := registryTokenCall(t, c, super, scopesReq("repository:onepony/x:delete")); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
		t.Fatalf("cross-namespace: status %d error %v", code, body["error"])
	}
}

// Whoever holds the chassis's own credentials (basic auth, open dev) is an
// operator here as on every other operator-only endpoint — and is held to
// the namespace and the pin like everyone else.
func TestRegistryTokenBasicAuthOperator(t *testing.T) {
	c, _ := newRegistryTokenController(t)
	for _, src := range []string{"basic", "open"} {
		op := registryCaller{slug: "acme", tenantID: "tnt_acme", caps: []string{"admin:all"}, source: src}
		if code, body := registryTokenCall(t, c, op, scopesReq("repository:acme/x:delete", "registry:catalog:*")); code != http.StatusOK {
			t.Fatalf("%s operator: status %d body %v", src, code, body)
		}
		if code, body := registryTokenCall(t, c, op, scopesReq("repository:onepony/x:push")); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
			t.Fatalf("%s operator, another namespace: status %d error %v", src, code, body["error"])
		}
		txco := registryCaller{slug: "txco", tenantID: "tnt_not_pinned", caps: []string{"admin:all"}, source: src}
		if code, body := registryTokenCall(t, c, txco, scopesReq("repository:txco/x:push")); code != http.StatusForbidden || body["error"] != "namespace_forbidden" {
			t.Fatalf("%s operator, unpinned txco: status %d error %v", src, code, body["error"])
		}
	}
	// A signed tenant admin is not an operator, whatever its capabilities.
	admin := registryCaller{slug: "acme", tenantID: "tnt_acme", caps: []string{"admin:all"}}
	if code, body := registryTokenCall(t, c, admin, scopesReq("repository:acme/x:delete")); code != http.StatusForbidden || body["error"] != "capability_missing" {
		t.Fatalf("signed admin:all delete: status %d error %v", code, body["error"])
	}
}

func TestRegistryTokenSuspendedTenant(t *testing.T) {
	c, _ := newRegistryTokenController(t)
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO tenant_runtime_state (tenant_id, enabled, suspended, deny_status, deny_reason) VALUES
		 ('tnt_acme', 1, 1, 402, 'payment_required'), ('tnt_off', 0, 0, 403, 'disabled')`); err != nil {
		t.Fatal(err)
	}
	if code, body := registryTokenCall(t, c, acmeOwner, scopesReq("repository:acme/x:pull,push")); code != http.StatusForbidden || body["error"] != "tenant_suspended" {
		t.Fatalf("suspended push: status %d error %v", code, body["error"])
	}
	off := registryCaller{slug: "off", tenantID: "tnt_off", caps: auth.TenantOwnerCaps()}
	if code, body := registryTokenCall(t, c, off, scopesReq("repository:off/x:push")); code != http.StatusForbidden || body["error"] != "tenant_suspended" {
		t.Fatalf("disabled push: status %d error %v", code, body["error"])
	}
	// Reading its own packages is not publishing.
	if code, body := registryTokenCall(t, c, acmeOwner, scopesReq("repository:acme/x:pull")); code != http.StatusOK {
		t.Fatalf("suspended pull: status %d body %v", code, body)
	}
	// An operator acting on the tenant is not held to its suspension.
	super := registryCaller{slug: "acme", tenantID: "tnt_acme", superAdmin: true}
	if code, body := registryTokenCall(t, c, super, scopesReq("repository:acme/x:push")); code != http.StatusOK {
		t.Fatalf("super-admin on a suspended tenant: status %d body %v", code, body)
	}
}

func TestRegistryTokenDisabled(t *testing.T) {
	c := newRuntimeStateTestController(t)
	code, body := registryTokenCall(t, c, acmeOwner, scopesReq("repository:acme/x:push"))
	if code != http.StatusNotFound || body["error"] != "registry_token_disabled" {
		t.Fatalf("status %d error %v, want 404 registry_token_disabled", code, body["error"])
	}
}

// Boot wiring: a complete configuration enables the endpoint; anything
// short of one leaves it off without stopping the chassis.
func TestResolveRegistryToken(t *testing.T) {
	keyPEM, certPEM, _ := registryTokenPair(t, time.Now().Add(365*24*time.Hour))
	dir := t.TempDir()
	keyPath, certPath := filepath.Join(dir, "token.key"), filepath.Join(dir, "root.crt")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	_, otherCert, _ := registryTokenPair(t, time.Now().Add(time.Hour))
	b64 := base64.StdEncoding.EncodeToString

	base := config.Config{Personalities: "admin", RegistryTokenIssuer: "https://admin.test", RegistryTokenService: testRegistryService}
	with := func(f func(*config.Config)) config.Config { cfg := base; f(&cfg); return cfg }

	for _, tc := range []struct {
		name string
		conf config.Config
		on   bool
	}{
		{"nothing configured", config.Config{Personalities: "admin"}, false},
		{"files", with(func(c *config.Config) { c.RegistryTokenKeyPath, c.RegistryTokenCertPath = keyPath, certPath }), true},
		{"base64", with(func(c *config.Config) { c.RegistryTokenKeyB64, c.RegistryTokenCertB64 = b64(keyPEM), b64(certPEM) }), true},
		{"base64 wins over a bad path", with(func(c *config.Config) {
			c.RegistryTokenKeyPath, c.RegistryTokenKeyB64 = "/nonexistent", b64(keyPEM)
			c.RegistryTokenCertPath = certPath
		}), true},
		{"pinned namespaces", with(func(c *config.Config) {
			c.RegistryTokenKeyPath, c.RegistryTokenCertPath = keyPath, certPath
			c.RegistryTokenPlatformNS = []string{"txco=tnt_txco", " library = tnt_lib "}
		}), true},
		{"no key", with(func(c *config.Config) { c.RegistryTokenCertPath = certPath }), false},
		{"no issuer", config.Config{Personalities: "admin", RegistryTokenService: testRegistryService, RegistryTokenKeyPath: keyPath, RegistryTokenCertPath: certPath}, false},
		{"no service", config.Config{Personalities: "admin", RegistryTokenIssuer: "i", RegistryTokenKeyPath: keyPath, RegistryTokenCertPath: certPath}, false},
		{"missing key file", with(func(c *config.Config) { c.RegistryTokenKeyPath, c.RegistryTokenCertPath = "/nonexistent", certPath }), false},
		{"certificate for another key", with(func(c *config.Config) { c.RegistryTokenKeyB64, c.RegistryTokenCertB64 = b64(keyPEM), b64(otherCert) }), false},
		{"malformed pin", with(func(c *config.Config) {
			c.RegistryTokenKeyPath, c.RegistryTokenCertPath = keyPath, certPath
			c.RegistryTokenPlatformNS = []string{"txco"}
		}), false},
	} {
		c := newTestController(t, tc.conf)
		c.resolveRegistryToken()
		if got := c.registrySigner != nil; got != tc.on {
			t.Errorf("%s: enabled = %v, want %v", tc.name, got, tc.on)
		}
		if tc.name == "pinned namespaces" {
			want := map[string]string{"txco": "tnt_txco", "library": "tnt_lib"}
			if !reflect.DeepEqual(c.registryPlatformNS, want) {
				t.Errorf("pins = %v, want %v", c.registryPlatformNS, want)
			}
		}
	}
}

// The route is on the tenant subrouter, so an unknown tenant is a 404
// before the handler: a caller cannot mint for a namespace that is no
// tenant's. Pinned here against the handler's own contract — it trusts the
// slug the middleware resolved, so it must never be reached without one.
func TestRegistryTokenNeedsResolvedTenant(t *testing.T) {
	c, _ := newRegistryTokenController(t)
	code, body := registryTokenCall(t, c, registryCaller{slug: "", tenantID: ""}, scopesReq("repository:acme/x:push"))
	if code != http.StatusInternalServerError || body["error"] != "tenant_id_missing" {
		t.Fatalf("status %d error %v", code, body["error"])
	}
}
