package connection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// A fake provider: a token destination, an account url and an api origin,
// all on one loopback server, counting what reaches each and refusing what
// must never arrive there.
type fakeProvider struct {
	srv          *httptest.Server
	challenge    atomic.Value // the code_challenge the authorize URL carried
	tokens       atomic.Int64
	exchanges    atomic.Int64
	refreshes    atomic.Int64
	refreshMode  atomic.Value // "ok" | "invalid" | "fail"
	expiresIn    atomic.Int64
	lastAPIAuth  atomic.Value
	secretLeaked atomic.Bool // the client secret reached an api or account url
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{}
	f.refreshMode.Store("ok")
	f.expiresIn.Store(100)
	f.lastAPIAuth.Store("")
	leak := func(r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_secret") != "" || r.Header.Get("Authorization") == "" || strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			f.secretLeaked.Store(true)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "s3cret" {
			w.WriteHeader(401)
			_, _ = fmt.Fprint(w, `{"error":"invalid_client"}`)
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.exchanges.Add(1)
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "good-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge.Load().(string) {
				// GitHub says no with a 200.
				_, _ = fmt.Fprint(w, `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`)
				return
			}
		case "refresh_token":
			f.refreshes.Add(1)
			switch f.refreshMode.Load().(string) {
			case "invalid":
				w.WriteHeader(400)
				_, _ = fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			case "fail":
				w.WriteHeader(502)
				return
			}
			if !strings.HasPrefix(r.Form.Get("refresh_token"), "rt-") {
				w.WriteHeader(400)
				_, _ = fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
		default:
			w.WriteHeader(400)
			return
		}
		n := f.tokens.Add(1) // only an issued token counts
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("at-%d", n), "refresh_token": fmt.Sprintf("rt-%d", n), "token_type": "bearer",
			"expires_in": f.expiresIn.Load(), "refresh_token_expires_in": 15897600, "scope": "repo read:user",
		})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		leak(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id": 583231, "login": "monalisa"}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		leak(r)
		f.lastAPIAuth.Store(r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/redirect") {
			http.Redirect(w, r, "https://evil.example/", http.StatusFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type fakeDecls struct {
	mu        sync.Mutex
	stack     string
	provider  *Provider
	consumers map[string]*Consumer
}

func (d *fakeDecls) Provider(_ context.Context, _, name string) (string, *Provider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if name != "github" || d.provider == nil {
		return "", nil, ErrNotDeclared
	}
	return d.stack, d.provider, nil
}

func (d *fakeDecls) Consumer(_ context.Context, _, stack, provider string) (*Consumer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.consumers[stack]
	if provider != "github" || c == nil {
		return nil, ErrNotDeclared
	}
	return c, nil
}

type fakeSecrets map[string]string

func (s fakeSecrets) Materialize(_ context.Context, _, _, name string) ([]byte, error) {
	v, ok := s[name]
	if !ok {
		return nil, errors.New("not set")
	}
	return []byte(v), nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	t     *testing.T
	fp    *fakeProvider
	decls *fakeDecls
	store *Store
	svc   *Service
	clock *clock
	ctx   context.Context
}

func newRig(t *testing.T) *rig {
	t.Helper()
	fp := newFakeProvider(t)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "auth.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	authntest.ApplyAuthSchema(t, db, "sqlite")
	mk, err := secrets.NewInlineMasterKey(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, registry.SQLite, mk)
	provider, err := ParseProvider([]byte(fmt.Sprintf(`title: GitHub
kind: oauth2
issuer: %[1]s
destinations:
  authorization: %[1]s/authorize
  token: %[1]s/token
  api: [%[1]s]
client_id: cid
client_secret: secret:GH_SECRET
pkce: true
account: {url: %[1]s/user, subject: id, name: login}
consumers:
  tools-api: [api.github.read]
`, fp.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := ParseConsumer([]byte("provider: github\norigins: [" + fp.srv.URL + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
	decls := &fakeDecls{stack: "tools-api", provider: provider, consumers: map[string]*Consumer{"tools-api": consumer}}
	signer := testSigner(t)
	node, _ := egress.Open("open", egress.Config{})
	svc, err := New(store, decls, fakeSecrets{"GH_SECRET": "s3cret"}, signer, Config{
		Node: node, Open: true, Logger: zap.NewNop(),
		SlugToID: func(_ context.Context, slug string) (string, error) { return "tnt_" + slug, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	svc.SetClock(c.now)
	return &rig{t: t, fp: fp, decls: decls, store: store, svc: svc, clock: c, ctx: context.Background()}
}

// connect runs a whole sign-in for a principal and returns the row.
func (r *rig) connect(principal string) Row {
	r.t.Helper()
	start, err := r.svc.Start(r.ctx, StartInput{
		Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: principal, Provider: "github",
		RedirectURI: "https://ranch.example/connect/github/callback", Host: "ranch.example", Initiator: "owner@example.com",
	})
	if err != nil {
		r.t.Fatalf("start: %v", err)
	}
	u, _ := url.Parse(start.URL)
	r.fp.challenge.Store(u.Query().Get("code_challenge"))
	fin, ferr := r.svc.Finish(r.ctx, r.callback(start, "good-code"))
	if ferr != nil {
		r.t.Fatalf("finish: %v", ferr)
	}
	return fin.Row
}

func cookieValue(line string) string {
	v := strings.TrimPrefix(strings.SplitN(line, ";", 2)[0], CookieName+"=")
	return v
}

func (r *rig) callback(start StartResult, code string) FinishInput {
	u, _ := url.Parse(start.URL)
	return FinishInput{
		Tenant: "acme", TenantID: "tnt_acme", Stack: "loop/canary", Host: "ranch.example", Path: "/connect/github/callback",
		Code: code, State: u.Query().Get("state"), Cookie: cookieValue(start.Cookie),
	}
}

func (r *rig) use(principal, stack, capability, path string) (UseResult, *Error) {
	u, _ := url.Parse(r.fp.srv.URL + path)
	return r.svc.Use(r.ctx, UseInput{Tenant: "acme", Stack: stack, Provider: "github", Principal: principal, Capability: capability, URL: u})
}

func (r *rig) row(principal string) Row {
	r.t.Helper()
	row, err := r.store.Get(r.ctx, "tnt_acme", principal, "github", "")
	if err != nil {
		r.t.Fatalf("row: %v", err)
	}
	return row
}

func (r *rig) waitGeneration(principal string, atLeast int64) Row {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		row := r.row(principal)
		if row.Generation >= atLeast && row.ClaimID == "" {
			return row
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("generation %d not reached: %+v", atLeast, row)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartAndFinish(t *testing.T) {
	r := newRig(t)
	start, err := r.svc.Start(r.ctx, StartInput{
		Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: "service:pony-a", Provider: "github",
		RedirectURI: "https://ranch.example/connect/github/callback", Host: "ranch.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, perr := url.Parse(start.URL)
	if perr != nil || u.Path != "/authorize" {
		t.Fatalf("url %q", start.URL)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("response_type") != "code" || q.Get("redirect_uri") != "https://ranch.example/connect/github/callback" ||
		q.Get("state") == "" || q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("scope") != "" {
		t.Errorf("query %v", q)
	}
	if !strings.Contains(start.Cookie, "Secure") || cookieValue(start.Cookie) == "" {
		t.Errorf("cookie %q", start.Cookie)
	}
	r.fp.challenge.Store(q.Get("code_challenge"))

	// The wrong browser: refused, and the flow is spent.
	in := r.callback(start, "good-code")
	in.Cookie = "someone-elses"
	if _, ferr := r.svc.Finish(r.ctx, in); ferr == nil || ferr.Code != CodeMismatch {
		t.Fatalf("wrong cookie: %v", ferr)
	}
	if _, ferr := r.svc.Finish(r.ctx, r.callback(start, "good-code")); ferr == nil || ferr.Code != CodeGone {
		t.Fatalf("a spent flow: %v", ferr)
	}
	if r.fp.exchanges.Load() != 0 {
		t.Error("no code may be exchanged before the flow is consumed and matched")
	}

	// A fresh sign-in completes.
	row := r.connect("service:pony-a")
	if row.Account != "monalisa" || row.Subject != "583231" || row.Status != StatusActive || row.Kind != CredentialOAuth2 ||
		row.Generation != 1 || row.ExpiresAt == nil || row.LifetimeS != 100 || row.CreatedBy != "loop" || len(row.Scopes) != 2 {
		t.Errorf("row %+v", row)
	}
	m, err2 := r.store.Open(r.ctx, row)
	if err2 != nil || m.Access != "at-1" || m.Refresh != "rt-1" {
		t.Errorf("material: %+v %v", m, err2)
	}
	// Replayed: gone.
	start2, _ := r.svc.Start(r.ctx, StartInput{Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: "service:pony-a", Provider: "github",
		RedirectURI: "https://ranch.example/connect/github/callback", Host: "ranch.example"})
	u2, _ := url.Parse(start2.URL)
	r.fp.challenge.Store(u2.Query().Get("code_challenge"))
	if _, ferr := r.svc.Finish(r.ctx, r.callback(start2, "good-code")); ferr != nil {
		t.Fatal(ferr)
	}
	if _, ferr := r.svc.Finish(r.ctx, r.callback(start2, "good-code")); ferr == nil || ferr.Code != CodeGone {
		t.Errorf("replay: %v", ferr)
	}
	if r.row("service:pony-a").Generation != 2 {
		t.Error("a reconnect re-seals at the next generation")
	}
	if r.fp.secretLeaked.Load() {
		t.Error("the client secret reached an api or account url")
	}
}

func TestFinishRefusals(t *testing.T) {
	r := newRig(t)
	fresh := func() (StartResult, FinishInput) {
		start, err := r.svc.Start(r.ctx, StartInput{Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: "service:pony-a", Provider: "github",
			RedirectURI: "https://ranch.example/connect/github/callback", Host: "ranch.example"})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(start.URL)
		r.fp.challenge.Store(u.Query().Get("code_challenge"))
		return start, r.callback(start, "good-code")
	}
	_, in := fresh()
	in.State = "v1.forged.state"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeBadState {
		t.Errorf("forged state: %v", err)
	}
	_, in = fresh()
	in.Stack = "ingest"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeMismatch {
		t.Errorf("another stack: %v", err)
	}
	_, in = fresh()
	in.Path = "/connect/slack/callback"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeMismatch {
		t.Errorf("another path: %v", err)
	}
	_, in = fresh()
	in.Iss = "https://evil.example"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeMismatch {
		t.Errorf("wrong iss: %v", err)
	}
	_, in = fresh()
	in.Iss = r.fp.srv.URL
	if _, err := r.svc.Finish(r.ctx, in); err != nil {
		t.Errorf("the provider's own iss: %v", err)
	}
	_, in = fresh()
	in.ProviderError = "access_denied"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeRefused {
		t.Errorf("provider error: %v", err)
	}
	_, in = fresh()
	in.Code = "bad-code"
	if _, err := r.svc.Finish(r.ctx, in); err == nil || err.Code != CodeRefused {
		t.Errorf("bad code: %v", err)
	}
	// Expired: the clock moves past the flow's life.
	start, _ := fresh()
	r.clock.advance(FlowTTL + time.Second)
	if _, err := r.svc.Finish(r.ctx, r.callback(start, "good-code")); err == nil || err.Code != CodeExpired {
		t.Errorf("expired: %v", err)
	}
	// Two callbacks racing on one flow: exactly one gets through.
	start, _ = fresh()
	var ok, gone atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.svc.Finish(r.ctx, r.callback(start, "good-code"))
			switch {
			case err == nil:
				ok.Add(1)
			case err.Code == CodeGone:
				gone.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || gone.Load() != 7 {
		t.Errorf("race: ok=%d gone=%d", ok.Load(), gone.Load())
	}
}

func TestStartRefusals(t *testing.T) {
	r := newRig(t)
	base := StartInput{Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: "service:pony-a", Provider: "github",
		RedirectURI: "https://ranch.example/connect/github/callback", Host: "ranch.example"}
	for name, edit := range map[string]func(*StartInput) string{
		"unknown provider": func(in *StartInput) string { in.Provider = "slack"; return CodeNotDeclared },
		"bad provider":     func(in *StartInput) string { in.Provider = "Git Hub"; return CodeInvalidArg },
		"installation":     func(in *StartInput) string { in.Kind = CredentialInstallation; return CodeUnsupported },
		"label":            func(in *StartInput) string { in.Label = "work"; return CodeUnsupported },
		"other host":       func(in *StartInput) string { in.Host = "other.example"; return CodeInvalidArg },
		"no stack":         func(in *StartInput) string { in.Stack = ""; return CodeInvalidArg },
	} {
		in := base
		want := edit(&in)
		if _, err := r.svc.Start(r.ctx, in); err == nil || err.Code != want {
			t.Errorf("%s: %v, want %s", name, err, want)
		}
	}
	// A client secret that is not set.
	r.svc.secrets = fakeSecrets{}
	if _, err := r.svc.Start(r.ctx, base); err == nil || err.Code != CodeNoClient {
		t.Errorf("no client secret: %v", err)
	}
}

func TestUseIsolationAndChecks(t *testing.T) {
	r := newRig(t)
	r.connect("service:pony-a")

	res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
	if err != nil || res.Header != "authorization" || res.Value != "Bearer at-1" || res.Token != "at-1" {
		t.Fatalf("use = %+v, %v", res, err)
	}
	// The request itself, through the service's client, carries the token
	// and follows no redirect.
	u, _ := url.Parse(r.fp.srv.URL + "/api/redirect")
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set(res.Header, res.Value)
	resp, rerr := r.svc.HTTPClient(u).Do(req)
	if rerr != nil {
		t.Fatal(rerr)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || r.fp.lastAPIAuth.Load().(string) != "Bearer at-1" {
		t.Errorf("redirect followed, or token not sent: %d %q", resp.StatusCode, r.fp.lastAPIAuth.Load())
	}

	for name, in := range map[string]struct {
		principal, stack, capability, path, want string
	}{
		"another principal":   {"service:pony-b", "tools-api", "api.github.read", "/api/x", CodeMissing},
		"a called helper":     {"service:pony-a", "helper", "api.github.read", "/api/x", CodeDenied},
		"another capability":  {"service:pony-a", "tools-api", "api.slack.read", "/api/x", CodeDenied},
		"no principal pinned": {"", "tools-api", "api.github.read", "/api/x", CodeDenied},
		"no capability":       {"service:pony-a", "tools-api", "", "/api/x", CodeDenied},
	} {
		if _, err := r.use(in.principal, in.stack, in.capability, in.path); err == nil || err.Code != in.want {
			t.Errorf("%s: %v, want %s", name, err, in.want)
		}
	}
	// An undeclared destination: another origin, or the declared host on
	// another port.
	for _, raw := range []string{"https://evil.example/api/x", strings.Replace(r.fp.srv.URL, "127.0.0.1:", "127.0.0.1:1", 1) + "/x"} {
		u, _ := url.Parse(raw)
		if _, err := r.svc.Use(r.ctx, UseInput{Tenant: "acme", Stack: "tools-api", Provider: "github", Principal: "service:pony-a", Capability: "api.github.read", URL: u}); err == nil || err.Code != CodeDestination {
			t.Errorf("%s: %v, want destination", raw, err)
		}
	}
	// A consumer of another kind: the row is oauth2.
	r.decls.mu.Lock()
	r.decls.consumers["tools-git"] = &Consumer{Provider: "github", Kind: CredentialInstallation, Origins: []string{r.fp.srv.URL}, Use: Use{Header: "authorization", Format: "Bearer {}"}, origins: r.decls.consumers["tools-api"].origins}
	r.decls.provider.Consumers["tools-git"] = []string{"api.github.read"}
	r.decls.mu.Unlock()
	if _, err := r.use("service:pony-a", "tools-git", "api.github.read", "/api/x"); err == nil || err.Code != CodeMissing {
		t.Errorf("another credential kind: %v", err)
	}
	// A row copied onto another principal does not open.
	if _, err := r.store.DB.Exec(`INSERT INTO connections (id, tenant_id, principal_id, provider, label, kind, subject, account, status, scopes, generation, expires_at, lifetime_s, refresh_expires_at, material, created_by, created_at, updated_at)
		SELECT 'con_copy', tenant_id, 'service:pony-b', provider, label, kind, subject, account, status, scopes, generation, expires_at, lifetime_s, refresh_expires_at, material, created_by, created_at, updated_at
		  FROM connections WHERE principal_id = 'service:pony-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.use("service:pony-b", "tools-api", "api.github.read", "/api/x"); err == nil || err.Code != CodeUnavailable {
		t.Errorf("a copied row: %v, want unavailable", err)
	}
	// The provider's file moves to another stack: the identity is the
	// provider's name, so the row still opens.
	r.decls.mu.Lock()
	r.decls.stack = "providers"
	r.decls.mu.Unlock()
	if res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil || res.Value != "Bearer at-1" {
		t.Errorf("after the provider moved: %+v %v", res, err)
	}
	// Revoked: missing.
	if _, removed, err := r.svc.Revoke(r.ctx, "acme", "tnt_acme", "service:pony-a", "github"); err != nil || !removed {
		t.Fatalf("revoke: %v %v", removed, err)
	}
	if _, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err == nil || err.Code != CodeMissing {
		t.Errorf("after revoke: %v", err)
	}
	if _, removed, _ := r.svc.Revoke(r.ctx, "acme", "tnt_acme", "service:pony-a", "github"); removed {
		t.Error("revoking twice")
	}
	if r.fp.secretLeaked.Load() {
		t.Error("the client secret reached an api url")
	}
}

func TestRenewalOnceUnderConcurrency(t *testing.T) {
	r := newRig(t)
	r.connect("service:pony-a")
	// lifetime 100s, margin 50s: not due at +40s.
	r.clock.advance(40 * time.Second)
	if res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil || res.Value != "Bearer at-1" {
		t.Fatalf("before the margin: %+v %v", res, err)
	}
	if r.fp.refreshes.Load() != 0 {
		t.Fatal("renewed before its margin")
	}
	// Due at +60s, still valid: everyone gets the current token now, and
	// exactly one renewal happens.
	r.clock.advance(20 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
			if err != nil || !strings.HasPrefix(res.Value, "Bearer at-") {
				t.Errorf("use during renewal: %+v %v", res, err)
			}
		}()
	}
	wg.Wait()
	row := r.waitGeneration("service:pony-a", 2)
	if r.fp.refreshes.Load() != 1 {
		t.Errorf("refreshes = %d, want 1", r.fp.refreshes.Load())
	}
	m, _ := r.store.Open(r.ctx, row)
	if m.Access != "at-2" || m.Refresh != "rt-2" || row.ExpiresAt == nil || row.Status != StatusActive {
		t.Errorf("renewed row: %+v %+v", row, m)
	}
	if res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil || res.Value != "Bearer at-2" {
		t.Errorf("after renewal: %+v %v", res, err)
	}
}

func TestRenewalWhenExpired(t *testing.T) {
	r := newRig(t)
	r.connect("service:pony-a")
	r.clock.advance(200 * time.Second)
	// Expired: the use renews synchronously and gets the new token.
	res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
	if err != nil || res.Value != "Bearer at-2" {
		t.Fatalf("expired use: %+v %v", res, err)
	}
	if r.fp.refreshes.Load() != 1 || r.row("service:pony-a").Generation != 2 {
		t.Error("one synchronous renewal")
	}
	// The provider refuses the next one: reconnect, and no request after.
	r.fp.refreshMode.Store("invalid")
	r.clock.advance(200 * time.Second)
	if _, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err == nil || err.Code != CodeReconnect {
		t.Fatalf("invalid_grant: %v", err)
	}
	if r.row("service:pony-a").Status != StatusReconnect {
		t.Error("status must be reconnect")
	}
	before := r.fp.refreshes.Load()
	if _, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err == nil || err.Code != CodeReconnect {
		t.Errorf("still reconnect: %v", err)
	}
	if r.fp.refreshes.Load() != before {
		t.Error("a reconnect row sends nothing")
	}
	// Reconnecting replaces it.
	r.fp.refreshMode.Store("ok")
	row := r.connect("service:pony-a")
	if row.Status != StatusActive || row.Generation != 3 {
		t.Errorf("after reconnect: %+v", row)
	}
}

func TestStaleClaimAndFencing(t *testing.T) {
	r := newRig(t)
	row := r.connect("service:pony-a")
	// Another node claimed and never finished: the claim is past its lease.
	if ok, err := r.store.Claim(r.ctx, row.ID, row.Generation, "clm_dead", r.clock.now().Add(-time.Second), StatusActive); err != nil || !ok {
		t.Fatal("claim")
	}
	// Nobody takes it over; the row is uncertain; the valid token still serves.
	res, uerr := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
	if uerr != nil || res.Value != "Bearer at-1" {
		t.Fatalf("use with a stale claim: %+v %v", res, uerr)
	}
	if got := r.row("service:pony-a"); got.Status != StatusUncertain || got.ClaimID != "" {
		t.Errorf("after a stale claim: %+v", got)
	}
	if r.fp.refreshes.Load() != 0 {
		t.Error("an expired claim must not start a renewal (on_uncertain: reconnect)")
	}
	// The dead claimant comes back late: fenced.
	if _, err := r.store.Renew(r.ctx, row, "clm_dead", Material{Access: "late"}, nil, nil, 0); !errors.Is(err, ErrFenced) {
		t.Errorf("late write: %v, want fenced", err)
	}
	if err := r.store.SetStatus(r.ctx, row, "clm_dead", StatusReconnect); !errors.Is(err, ErrFenced) {
		t.Errorf("late verdict: %v, want fenced", err)
	}
	// The token expires: with on_uncertain reconnect, the person reconnects.
	r.clock.advance(200 * time.Second)
	if _, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err == nil || err.Code != CodeReconnect {
		t.Errorf("uncertain and expired: %v", err)
	}
	if r.fp.refreshes.Load() != 0 {
		t.Error("the old refresh token must never be re-sent under reconnect")
	}
}

func TestUncertainRetry(t *testing.T) {
	r := newRig(t)
	r.decls.provider.Refresh.OnUncertain = OnUncertainRetry
	r.decls.provider.Refresh.Rotation = RotationReusable
	r.connect("service:pony-a")
	// The provider fails the renewal (a 5xx): uncertain; the token serves.
	r.fp.refreshMode.Store("fail")
	r.clock.advance(60 * time.Second)
	res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
	if err != nil || res.Value != "Bearer at-1" {
		t.Fatalf("use: %+v %v", res, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.row("service:pony-a").Status != StatusUncertain {
		if time.Now().After(deadline) {
			t.Fatalf("status %s, want uncertain", r.row("service:pony-a").Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Under retry, the next due use tries once more, and the provider is
	// back: renewed.
	r.fp.refreshMode.Store("ok")
	r.clock.advance(200 * time.Second)
	res, err = r.use("service:pony-a", "tools-api", "api.github.read", "/api/x")
	if err != nil || res.Value != "Bearer at-2" {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if r.fp.refreshes.Load() != 2 || r.row("service:pony-a").Status != StatusActive {
		t.Errorf("refreshes %d status %s", r.fp.refreshes.Load(), r.row("service:pony-a").Status)
	}
}

func TestShortTokenNotRenewedEarly(t *testing.T) {
	r := newRig(t)
	r.fp.expiresIn.Store(30)
	r.connect("service:pony-a")
	// Half-life beats the 60-second floor: at +10s nothing is due.
	r.clock.advance(10 * time.Second)
	if res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil || res.Value != "Bearer at-1" {
		t.Fatalf("use: %+v %v", res, err)
	}
	if r.fp.refreshes.Load() != 0 {
		t.Error("renewed in the first half-life")
	}
	r.clock.advance(6 * time.Second)
	if _, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil {
		t.Fatal(err)
	}
	r.waitGeneration("service:pony-a", 2)
	if r.fp.refreshes.Load() != 1 {
		t.Error("due in the last half")
	}
}

func TestNoExpiryNeverRenews(t *testing.T) {
	r := newRig(t)
	r.fp.expiresIn.Store(0)
	row := r.connect("service:pony-a")
	if row.ExpiresAt != nil || row.LifetimeS != 0 {
		t.Fatalf("row %+v", row)
	}
	r.clock.advance(1000 * time.Hour)
	if res, err := r.use("service:pony-a", "tools-api", "api.github.read", "/api/x"); err != nil || res.Value != "Bearer at-1" || r.fp.refreshes.Load() != 0 {
		t.Errorf("a token without expiry: %+v %v", res, err)
	}
}

func TestStrictClientRefusesLoopback(t *testing.T) {
	r := newRig(t)
	node, _ := egress.Open("open", egress.Config{})
	strict, err := newClients(node, false)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := ParseOrigin(r.fp.srv.URL)
	if _, err := strict.For(o).Get(r.fp.srv.URL + "/api/x"); err == nil {
		t.Error("a credentialed dial to a private address must be refused on a node that is not open")
	}
	open, _ := newClients(node, true)
	resp, err := open.For(o).Get(r.fp.srv.URL + "/api/x")
	if err != nil {
		t.Fatalf("a development node reaches loopback: %v", err)
	}
	resp.Body.Close()
}

func TestMaterialBinding(t *testing.T) {
	mk, _ := secrets.NewInlineMasterKey(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	col, err := sealMaterial(mk, Material{Access: "a", Refresh: "r"}, "t", "p", "github", "", "con_1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if m, err := openMaterial(mk, col, "t", "p", "github", "", "con_1", 1); err != nil || m.Access != "a" || m.Refresh != "r" {
		t.Fatalf("round trip: %+v %v", m, err)
	}
	for name, args := range map[string][6]any{
		"principal":  {"t", "q", "github", "", "con_1", int64(1)},
		"provider":   {"t", "p", "slack", "", "con_1", int64(1)},
		"generation": {"t", "p", "github", "", "con_1", int64(2)},
		"tenant":     {"u", "p", "github", "", "con_1", int64(1)},
		"row":        {"t", "p", "github", "", "con_2", int64(1)},
	} {
		if _, err := openMaterial(mk, col, args[0].(string), args[1].(string), args[2].(string), args[3].(string), args[4].(string), args[5].(int64)); err == nil {
			t.Errorf("%s: opened under another identity", name)
		}
	}
	if strings.Contains(col, "\"a\"") || strings.Contains(col, "r\"") {
		t.Error("the column must not hold cleartext")
	}
}

func TestParseTokenAnswer(t *testing.T) {
	a := parseTokenAnswer("application/x-www-form-urlencoded", []byte("access_token=x&token_type=bearer&expires_in=28800&refresh_token=y&scope=repo"))
	if a.Access != "x" || a.Refresh != "y" || a.ExpiresIn != 28800 || a.Scope != "repo" {
		t.Errorf("form: %+v", a)
	}
	a = parseTokenAnswer("application/json; charset=utf-8", []byte(`{"access_token":"x","expires_in":"3600","error":""}`))
	if a.Access != "x" || a.ExpiresIn != 3600 || a.Err != "" {
		t.Errorf("json: %+v", a)
	}
	a = parseTokenAnswer("", []byte(`{"error":"bad_verification_code"}`))
	if a.Err != "bad_verification_code" {
		t.Errorf("bare json: %+v", a)
	}
	if got := classify(&url.Error{Err: &net.OpError{Op: "dial"}}); got != outcomeNotSent {
		t.Error("a dial error was not sent")
	}
	if got := classify(errors.New("read: connection reset")); got != outcomeUncertain {
		t.Error("anything else is uncertain")
	}
}
