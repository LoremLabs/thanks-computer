package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// fakeRegistry is just enough of an OCI registry for a package push and
// pull, shaped like the hosted one: reads are open, writes answer 401 with
// a challenge until the request carries a token the fake chassis issued.
type fakeRegistry struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]fakeManifest
	valid     map[string]bool // tokens accepted for a write
	// tokenService is what the well-known document names ("" → 404).
	tokenService string
	// wellKnownStatus, when set, is answered for the well-known document
	// instead of it (a failing edge).
	wellKnownStatus int
	// basic makes writes challenge for Basic instead of Bearer.
	basic bool
	// expireAfter, when > 0, revokes every token issued so far once that
	// many writes have been accepted: an expiry in the middle of a push.
	expireAfter int
	challenges  int
	writes      int
}

type fakeManifest struct {
	contentType string
	body        []byte
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{t: t, blobs: map[string][]byte{}, manifests: map[string]fakeManifest{}, valid: map[string]bool{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRegistry) host() string { return strings.TrimPrefix(r.srv.URL, "http://") }

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.URL.Path == registryWellKnownPath {
		if r.wellKnownStatus != 0 {
			w.WriteHeader(r.wellKnownStatus)
			return
		}
		if r.tokenService == "" {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token_service": r.tokenService})
		return
	}
	if req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	var name, kind, ref string
	for _, k := range []string{"/blobs/uploads/", "/blobs/", "/manifests/"} {
		if i := strings.LastIndex(rest, k); i >= 0 {
			name, kind, ref = rest[:i], k, rest[i+len(k):]
			break
		}
	}
	if name == "" {
		http.NotFound(w, req)
		return
	}

	write := req.Method != http.MethodGet && req.Method != http.MethodHead
	if write {
		tok := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if !r.valid[tok] {
			r.challenges++
			if r.basic {
				w.Header().Set("Www-Authenticate", `Basic realm="registry"`)
			} else {
				w.Header().Set("Www-Authenticate", fmt.Sprintf(
					`Bearer realm="http://127.0.0.1:1/unused",service=%q,scope="repository:%s:pull,push"`, r.host(), name))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.writes++
		if r.writes == r.expireAfter {
			r.valid = map[string]bool{}
		}
	}

	switch {
	case kind == "/blobs/uploads/" && req.Method == http.MethodPost:
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/session")
		w.WriteHeader(http.StatusAccepted)
	case kind == "/blobs/uploads/" && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		d := req.URL.Query().Get("digest")
		if d != digestOf(body) {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		r.blobs[name+"@"+d] = body
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	case kind == "/blobs/":
		body, ok := r.blobs[name+"@"+ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", ref)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case kind == "/manifests/" && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		m := fakeManifest{contentType: req.Header.Get("Content-Type"), body: body}
		d := digestOf(body)
		r.manifests[name+"@"+d] = m
		r.manifests[name+"@"+ref] = m
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	case kind == "/manifests/":
		m, ok := r.manifests[name+"@"+ref]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", m.contentType)
		w.Header().Set("Docker-Content-Digest", digestOf(m.body))
		w.Header().Set("Content-Length", fmt.Sprint(len(m.body)))
		if req.Method == http.MethodGet {
			_, _ = w.Write(m.body)
		}
	default:
		http.NotFound(w, req)
	}
}

// fakeChassis is the token endpoint: it records each request and issues a
// token the registry will accept.
type fakeChassis struct {
	srv *httptest.Server
	reg *fakeRegistry

	mu    sync.Mutex
	calls []chassisCall
	// refuse, when set, answers every request with this status and error.
	refuseStatus int
	refuseCode   string
}

type chassisCall struct {
	Tenant  string
	Signed  bool
	Service string
	Scopes  []string
}

func newFakeChassis(t *testing.T, reg *fakeRegistry) *fakeChassis {
	t.Helper()
	c := &fakeChassis{reg: reg}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) != 5 || parts[0] != "v1" || parts[1] != "tenants" || parts[3] != "registry" || parts[4] != "token" || req.Method != http.MethodPost {
			http.NotFound(w, req)
			return
		}
		var body struct {
			Service string   `json:"service"`
			Scopes  []string `json:"scopes"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		c.calls = append(c.calls, chassisCall{
			Tenant: parts[2], Signed: req.Header.Get("Signature-Input") != "",
			Service: body.Service, Scopes: body.Scopes,
		})
		w.Header().Set("Content-Type", "application/json")
		if c.refuseStatus != 0 {
			w.WriteHeader(c.refuseStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": c.refuseCode, "detail": map[string]any{"hint": "the server's reason"}})
			return
		}
		tok := fmt.Sprintf("tok-%d", len(c.calls))
		reg.mu.Lock()
		reg.valid[tok] = true
		reg.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "access_token": tok, "expires_in": 300})
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeChassis) seen() []chassisCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chassisCall(nil), c.calls...)
}

// registryTestEnv isolates everything the provider reads: profiles, docker
// credentials, the env overrides and the discovery cache.
func registryTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	for _, k := range []string{"TXCO_PROFILE", "TXCO_PRIVATE_KEY_PATH", "TXCO_OCI_USERNAME", "TXCO_OCI_PASSWORD"} {
		t.Setenv(k, "")
	}
	// Run from a directory with no workspace above it.
	wd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	tokenServices = sync.Map{}
	prev := registryTokenProfile
	registryTokenProfile = ""
	t.Cleanup(func() { registryTokenProfile = prev; tokenServices = sync.Map{} })
}

func packageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "txco.package.yaml"), []byte("name: hello\nversion: 0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// profileFor enrolls a signing profile at a chassis, for a tenant.
func profileFor(t *testing.T, name, chassisURL, tenant string) {
	t.Helper()
	writeProfileFixture(t, name, chassisURL, 0o600)
	mp, err := auth.MetaPath(name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := auth.LoadMeta(mp)
	if err != nil {
		t.Fatal(err)
	}
	m.DefaultTenant = tenant
	if err := auth.SaveMeta(mp, *m); err != nil {
		t.Fatal(err)
	}
}

func mustRef(t *testing.T, s string) source.ParsedRef {
	t.Helper()
	ref, err := source.ParseRef(s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// A publish to a registry whose tokens a chassis issues: the registry's
// challenge is answered by a token asked of that chassis, for the tenant in
// the package's first path segment, bound to this registry. Reading the
// package back asks the chassis nothing.
func TestRegistryTokenPublishAndPull(t *testing.T) {
	registryTestEnv(t)
	reg := newFakeRegistry(t)
	ch := newFakeChassis(t, reg)
	reg.tokenService = ch.srv.URL
	profileFor(t, "acme", ch.srv.URL, "acme")

	ref := mustRef(t, "oci://"+reg.host()+"/acme/hello:0.1.0")
	digest, err := source.Publish(context.Background(), packageDir(t), ref)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	calls := ch.seen()
	if len(calls) != 1 {
		t.Fatalf("chassis was asked %d times, want once: %+v", len(calls), calls)
	}
	want := chassisCall{Tenant: "acme", Signed: true, Service: reg.host(), Scopes: []string{"repository:acme/hello:pull,push"}}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("token request = %+v, want %+v", calls[0], want)
	}
	if _, ok := reg.manifests["acme/hello@0.1.0"]; !ok {
		t.Fatal("the registry has no acme/hello:0.1.0")
	}
	if reg.manifests["acme/hello@"+digest].body == nil {
		t.Fatalf("published digest %s is not the manifest's", digest)
	}

	// Pull it back: the fake's reads are open, so no credential is asked for.
	dest := t.TempDir()
	if _, err := source.Fetch(context.Background(), "oci://"+ref.Reference(), dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(b) != "hello\n" {
		t.Fatalf("fetched tree: %q, %v", b, err)
	}
	if n := len(ch.seen()); n != 1 {
		t.Fatalf("a pull asked the chassis for a token (%d calls)", n)
	}
}

// A token the registry stops accepting in the middle of a push is replaced
// by a fresh one, and a second push (the signature after a publish) asks
// again: nothing is remembered here beyond the client that asked.
func TestRegistryTokenIsRefetched(t *testing.T) {
	registryTestEnv(t)
	reg := newFakeRegistry(t)
	ch := newFakeChassis(t, reg)
	reg.tokenService = ch.srv.URL
	// Both blobs are up after four writes (a POST and a PUT each); the
	// manifest PUT that follows then meets a token that no longer works.
	reg.expireAfter = 4

	// No profile: a chassis on this machine is asked unsigned.
	ref := mustRef(t, "oci://"+reg.host()+"/default/hello:0.1.0")
	if _, err := source.Publish(context.Background(), packageDir(t), ref); err != nil {
		t.Fatalf("publish across an expiry: %v", err)
	}
	calls := ch.seen()
	if len(calls) < 2 {
		t.Fatalf("%d token requests, want a second after the first token expired", len(calls))
	}
	for _, c := range calls {
		if c.Signed || c.Tenant != "default" || c.Service != reg.host() {
			t.Fatalf("call = %+v, want unsigned, tenant default, this registry", c)
		}
	}
	before := len(calls)
	if _, err := source.Publish(context.Background(), packageDir(t), mustRef(t, "oci://"+reg.host()+"/default/hello:0.1.1")); err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if len(ch.seen()) == before {
		t.Fatal("a second publish reused the first one's token")
	}
}

// The tenant is the FIRST path segment, however deep the name.
func TestRegistryTokenNestedName(t *testing.T) {
	registryTestEnv(t)
	reg := newFakeRegistry(t)
	ch := newFakeChassis(t, reg)
	reg.tokenService = ch.srv.URL

	if _, err := source.Publish(context.Background(), packageDir(t), mustRef(t, "oci://"+reg.host()+"/onepony/tools/web:1")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	calls := ch.seen()
	if len(calls) != 1 || calls[0].Tenant != "onepony" || !reflect.DeepEqual(calls[0].Scopes, []string{"repository:onepony/tools/web:pull,push"}) {
		t.Fatalf("calls = %+v", calls)
	}
}

// A registry that names no token service, or challenges for Basic, is the
// docker-config path: the chassis is not consulted.
func TestRegistryTokenNotOurs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(reg *fakeRegistry, ch *fakeChassis)
	}{
		{"no well-known document", func(reg *fakeRegistry, ch *fakeChassis) {}},
		{"basic challenge", func(reg *fakeRegistry, ch *fakeChassis) { reg.tokenService, reg.basic = ch.srv.URL, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registryTestEnv(t)
			reg := newFakeRegistry(t)
			ch := newFakeChassis(t, reg)
			tc.setup(reg, ch)
			_, err := source.Publish(context.Background(), packageDir(t), mustRef(t, "oci://"+reg.host()+"/acme/hello:0.1.0"))
			if err == nil {
				t.Fatal("publish succeeded with no credentials")
			}
			if n := len(ch.seen()); n != 0 {
				t.Fatalf("the chassis was asked %d times", n)
			}
		})
	}
}

// An explicit TXCO_OCI_USERNAME outranks the token service.
func TestRegistryTokenEnvCredentialsWin(t *testing.T) {
	registryTestEnv(t)
	reg := newFakeRegistry(t)
	ch := newFakeChassis(t, reg)
	reg.tokenService = ch.srv.URL
	t.Setenv("TXCO_OCI_USERNAME", "publisher")
	t.Setenv("TXCO_OCI_PASSWORD", "pw")
	_, _ = source.Publish(context.Background(), packageDir(t), mustRef(t, "oci://"+reg.host()+"/acme/hello:0.1.0"))
	if n := len(ch.seen()); n != 0 {
		t.Fatalf("the chassis was asked %d times despite TXCO_OCI_USERNAME", n)
	}
}

// What the chassis refuses, the publisher reads; a chassis that is not a
// token service hands the challenge back to docker credentials.
func TestRegistryTokenRefusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   string
	}{
		{http.StatusNotFound, "tenant_not_found", `no tenant "acme"`},
		{http.StatusForbidden, "capability_missing", "may not publish under acme/: the server's reason"},
		{http.StatusForbidden, "namespace_forbidden", "registry token refused (namespace_forbidden): the server's reason"},
		{http.StatusForbidden, "tenant_suspended", "registry token refused (tenant_suspended)"},
		{http.StatusForbidden, "service_mismatch", "registry token refused (service_mismatch)"},
		{http.StatusUnauthorized, "invalid_signature", "did not accept the request"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			registryTestEnv(t)
			reg := newFakeRegistry(t)
			ch := newFakeChassis(t, reg)
			reg.tokenService = ch.srv.URL
			ch.refuseStatus, ch.refuseCode = tc.status, tc.code
			_, err := source.Publish(context.Background(), packageDir(t), mustRef(t, "oci://"+reg.host()+"/acme/hello:0.1.0"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
			if reg.writes != 0 {
				t.Fatal("a refused publish wrote to the registry")
			}
		})
	}

	t.Run("disabled", func(t *testing.T) {
		registryTestEnv(t)
		reg := newFakeRegistry(t)
		ch := newFakeChassis(t, reg)
		reg.tokenService = ch.srv.URL
		ch.refuseStatus, ch.refuseCode = http.StatusNotFound, "registry_token_disabled"
		handled, err := registryTokenError(&client.HTTPError{StatusCode: 404, Code: "registry_token_disabled"}, reg.host(), "acme", "p")
		if handled || err != nil {
			t.Fatalf("handled=%v err=%v, want the challenge handed back", handled, err)
		}
	})
}

func TestChassisOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://admin.thanks.computer":      "https://admin.thanks.computer",
		"https://Admin.Thanks.Computer:443/": "https://admin.thanks.computer",
		"https://admin.example.com:8443/x":   "https://admin.example.com:8443",
		"https://[2001:db8::1]:8443":         "https://[2001:db8::1]:8443",
		// Every spelling of this machine is one origin.
		"http://localhost:8081":  "http://localhost:8081",
		"http://127.0.0.1:8081/": "http://localhost:8081",
		"http://127.0.0.10:8081": "http://localhost:8081",
		"http://[::1]:8081":      "http://localhost:8081",
		"http://localhost:80":    "http://localhost",
	} {
		got, err := chassisOrigin(in)
		if err != nil || got != want {
			t.Errorf("chassisOrigin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// Plain http only to a loopback ADDRESS or `localhost` itself: a name
	// that merely should resolve there is the resolver's word, not ours.
	for _, bad := range []string{"", "admin.thanks.computer", "http://admin.thanks.computer", "http://10.0.0.5:8081",
		"http://dev.localhost:8081", "http://localhost.example.com:8081", "ftp://localhost",
		"https://user:pw@admin.thanks.computer", "http://user@localhost:8081", "https://"} {
		if got, err := chassisOrigin(bad); err == nil {
			t.Errorf("chassisOrigin(%q) = %q, want an error", bad, got)
		}
	}
	for _, eq := range [][2]string{
		{"http://127.0.0.1:8081", "http://localhost:8081"},
		{"http://[::1]:8081/", "http://localhost:8081"},
		{"https://admin.thanks.computer/", "https://admin.thanks.computer"},
	} {
		if !sameOrigin(eq[0], eq[1]) {
			t.Errorf("sameOrigin(%q, %q) = false", eq[0], eq[1])
		}
	}
	for _, ne := range [][2]string{
		{"https://admin.thanks.computer", "https://admin.example.com"},
		{"http://localhost:8081", "http://localhost:8082"},
		{"https://127.0.0.10:8443", "https://localhost0:8443"}, // a prefix is not a host
		{"https://localhost:8081", "http://localhost:8081"},
		{"", "http://localhost:8081"},
	} {
		if sameOrigin(ne[0], ne[1]) {
			t.Errorf("sameOrigin(%q, %q) = true", ne[0], ne[1])
		}
	}
}

// What a registry may and may not say about its token service, and the
// difference between "names none" (which the registry said) and "could not
// be asked" (which must never be read as none).
func TestFetchRegistryTokenService(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name, body string
		status     int
		local      bool
		want       string
		wantErr    string
	}{
		{"https service", `{"token_service":"https://admin.thanks.computer"}`, 200, false, "https://admin.thanks.computer", ""},
		{"a local registry may name a local chassis", `{"token_service":"http://127.0.0.1:8081"}`, 200, true, "http://localhost:8081", ""},
		{"a local registry may name a remote chassis", `{"token_service":"https://admin.thanks.computer"}`, 200, true, "https://admin.thanks.computer", ""},
		{"absent", `not found`, 404, false, "", ""},
		{"refused", `no`, 401, false, "", ""},
		{"empty document", `{}`, 200, false, "", ""},
		{"not JSON", `<html>`, 200, false, "", ""},
		// A remote registry cannot point the CLI at a port on this machine.
		{"a remote registry names a local chassis", `{"token_service":"http://localhost:9000"}`, 200, false, "", "not https"},
		{"plain http service", `{"token_service":"http://admin.example.com"}`, 200, true, "", "not https"},
		{"not a URL", `{"token_service":"admin.example.com"}`, 200, false, "", "not a URL"},
		{"the registry is failing", `bad gateway`, 502, false, "", "HTTP 502"},
	} {
		srv := serve(tc.status, tc.body)
		got, err := fetchRegistryTokenService(ctx, srv.Client(), srv.URL+registryWellKnownPath, tc.local)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
		if tc.wantErr == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.wantErr)
		}
	}
	// A registry that cannot be reached was not asked: an error, not "none".
	if got, err := fetchRegistryTokenService(ctx, http.DefaultClient, "http://127.0.0.1:1"+registryWellKnownPath, true); got != "" || err == nil {
		t.Errorf("unreachable: got %q, %v; want an error", got, err)
	}
}

// A registry whose discovery fails is asked again, not remembered as having
// no token service — and the publish fails saying so, instead of quietly
// trying docker credentials.
func TestRegistryTokenDiscoveryFailureIsNotCached(t *testing.T) {
	registryTestEnv(t)
	reg := newFakeRegistry(t)
	ch := newFakeChassis(t, reg)
	reg.tokenService = ch.srv.URL
	reg.wellKnownStatus = http.StatusBadGateway

	ref := mustRef(t, "oci://"+reg.host()+"/default/hello:0.1.0")
	_, err := source.Publish(context.Background(), packageDir(t), ref)
	if err == nil || !strings.Contains(err.Error(), "who issues its tokens") {
		t.Fatalf("err = %v, want the discovery failure", err)
	}
	if n := len(ch.seen()); n != 0 {
		t.Fatalf("the chassis was asked %d times", n)
	}
	reg.mu.Lock()
	reg.wellKnownStatus = 0
	reg.mu.Unlock()
	if _, err := source.Publish(context.Background(), packageDir(t), ref); err != nil {
		t.Fatalf("publish once discovery answers: %v", err)
	}
}

func TestScopesTenant(t *testing.T) {
	for _, tc := range []struct {
		scopes []string
		want   string
		err    string
	}{
		{[]string{"repository:acme/hello:pull,push"}, "acme", ""},
		{[]string{"repository:onepony/a/b:pull"}, "onepony", ""},
		{[]string{"repository:acme/a:pull,push", "repository:acme/b:pull"}, "acme", ""},
		{[]string{"repository:hello:pull,push"}, "", "no namespace"},
		{[]string{"repository:acme/a:push", "repository:other/b:pull"}, "", "two namespaces"},
	} {
		got, err := scopesTenant(tc.scopes)
		if tc.err == "" && (err != nil || got != tc.want) {
			t.Errorf("%v = %q, %v; want %q", tc.scopes, got, err, tc.want)
		}
		if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("%v: err = %v, want one containing %q", tc.scopes, err, tc.err)
		}
	}
}

func TestPickRegistryProfile(t *testing.T) {
	const svc = "https://admin.thanks.computer"
	const reg = "registry.thanks.computer"
	pick := func(t *testing.T, tenant string) (string, bool, error) {
		t.Helper()
		name, sg, err := pickRegistryProfile(svc, tenant, reg)
		return name, sg != nil, err
	}

	t.Run("the profile for the tenant, not the active one", func(t *testing.T) {
		registryTestEnv(t)
		profileFor(t, "cloud", svc, "mankins")
		profileFor(t, "onepony", svc, "onepony")
		profileFor(t, "local", "http://localhost:8081", "default")
		if err := auth.WriteActiveProfile("cloud"); err != nil {
			t.Fatal(err)
		}
		if name, signed, err := pick(t, "onepony"); err != nil || name != "onepony" || !signed {
			t.Fatalf("got %q signed=%v err=%v, want onepony", name, signed, err)
		}
		if name, _, err := pick(t, "mankins"); err != nil || name != "cloud" {
			t.Fatalf("got %q err=%v, want cloud", name, err)
		}
		// No profile is for this tenant: the active one, enrolled there, signs
		// (it may be a member of more than its default tenant).
		if name, _, err := pick(t, "txco"); err != nil || name != "cloud" {
			t.Fatalf("got %q err=%v, want the active profile cloud", name, err)
		}
	})

	t.Run("a named profile wins and must be enrolled there", func(t *testing.T) {
		registryTestEnv(t)
		profileFor(t, "cloud", svc, "mankins")
		profileFor(t, "onepony", svc, "onepony")
		profileFor(t, "local", "http://localhost:8081", "default")
		registryTokenProfile = "cloud"
		if name, _, err := pick(t, "onepony"); err != nil || name != "cloud" {
			t.Fatalf("--profile: got %q err=%v, want cloud", name, err)
		}
		registryTokenProfile = ""
		t.Setenv("TXCO_PROFILE", "cloud")
		if name, _, err := pick(t, "onepony"); err != nil || name != "cloud" {
			t.Fatalf("TXCO_PROFILE: got %q err=%v, want cloud", name, err)
		}
		t.Setenv("TXCO_PROFILE", "local")
		if _, _, err := pick(t, "onepony"); err == nil || !strings.Contains(err.Error(), "not enrolled") {
			t.Fatalf("a profile for another chassis: err = %v", err)
		}
	})

	t.Run("several, none for the tenant, none active", func(t *testing.T) {
		registryTestEnv(t)
		profileFor(t, "a", svc, "one")
		profileFor(t, "b", svc, "two")
		if err := auth.WriteActiveProfile(auth.ActiveNone); err != nil {
			t.Fatal(err)
		}
		if _, _, err := pick(t, "three"); err == nil || !strings.Contains(err.Error(), "pass --profile") {
			t.Fatalf("err = %v, want a request for --profile", err)
		}
	})

	t.Run("the only one enrolled there", func(t *testing.T) {
		registryTestEnv(t)
		profileFor(t, "work", svc, "one")
		profileFor(t, "local", "http://localhost:8081", "default")
		if name, _, err := pick(t, "three"); err != nil || name != "work" {
			t.Fatalf("got %q err=%v, want work", name, err)
		}
	})

	t.Run("none: a remote chassis needs a sign-in, and the message names no URL", func(t *testing.T) {
		registryTestEnv(t)
		profileFor(t, "local", "http://localhost:8081", "default")
		_, _, err := pick(t, "acme")
		if err == nil || !strings.Contains(err.Error(), "needs a txco profile") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "admin.thanks.computer") {
			t.Fatalf("the error repeats the URL the registry supplied: %v", err)
		}
	})

	t.Run("none: a chassis on this machine is asked unsigned", func(t *testing.T) {
		registryTestEnv(t)
		name, sg, err := pickRegistryProfile("http://localhost:8081", "default", "localhost:5555")
		if err != nil || name != "" || sg != nil {
			t.Fatalf("got %q signer=%v err=%v, want unsigned", name, sg, err)
		}
	})
}

func TestLoopbackRegistryBaseURL(t *testing.T) {
	for host, want := range map[string]string{
		"registry.thanks.computer": "https://registry.thanks.computer",
		"localhost:5555":           "http://localhost:5555",
		"127.0.0.1:5000":           "http://127.0.0.1:5000",
		"[::1]:5000":               "http://[::1]:5000",
		"reg.localhost":            "https://reg.localhost",
		"localhost.example.com":    "https://localhost.example.com",
		"10.0.0.5:5000":            "https://10.0.0.5:5000",
	} {
		if got := registryBaseURL(host); got != want {
			t.Errorf("registryBaseURL(%q) = %q, want %q", host, got, want)
		}
	}
}
