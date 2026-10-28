package git

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	gitcore "github.com/loremlabs/thanks-computer/chassis/git"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The door, over the durable store: Basic auth through the login resolver
// the heads share, a credential's scopes deciding fetch and push, the
// tenant from the hostname. The protocol itself is proven in handler_test.

type doorHarness struct {
	*harness
	ids   *authn.Store
	repos *gitcore.StoreRepos
}

func newDoorHarness(t *testing.T) *doorHarness {
	t.Helper()
	dir := t.TempDir()
	st, err := gitstore.OpenSQLiteFile(filepath.Join(dir, "git.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs, err := filestore.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	repos := gitcore.NewStoreRepos(st, fs, defaultLimits)
	conf := config.Config{
		Personalities: "web,git", GitPathPrefix: "/git", GitInsecureAuth: true, LoginRate: 100,
		GitMaxPackBytes: int(defaultLimits.MaxPackBytes), GitMaxObjectBytes: int(defaultLimits.MaxObjectBytes),
		GitMaxObjects: defaultLimits.MaxObjects, GitMaxRepoBytes: int(defaultLimits.MaxRepoBytes), GitTextOnly: true,
		GitRespTimeout: "10s", GitObserveSample: 1, GitObserveMaxInflight: 2,
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, repos, fakeResolver{"127.0.0.1": "acme"})
	ids := authntest.NewSQLiteStore(t)
	ctrl.SetAuth(authn.NewResolver(ids, authn.ResolverConfig{Rate: conf.LoginRate, TenantID: authntest.SameTenantID}))
	ctrl.Start()
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(func() { srv.Close(); ctrl.Stop(); cancel() })
	h := &harness{t: t, ctrl: ctrl, srv: srv, home: t.TempDir()}
	return &doorHarness{harness: h, ids: ids, repos: repos}
}

// grant binds username → principal in tenant acme and issues a password
// with the scopes.
func (d *doorHarness) grant(t *testing.T, principal, username, password string, scopes ...string) {
	t.Helper()
	p, err := authn.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	authntest.Grant(t, d.ids, "acme", p, username, password, scopes...)
}

// authURL puts the credentials in the clone URL, as a developer's
// credential helper would send them.
func (d *doorHarness) authURL(repo, user, pass string) string {
	u, _ := url.Parse(d.srv.URL)
	u.User = url.UserPassword(user, pass)
	return u.String() + "/git/" + repo + ".git"
}

func TestDoorScopesAndTenant(t *testing.T) {
	gitPath(t)
	d := newDoorHarness(t)
	ctx := context.Background()
	if _, _, err := d.repos.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "pony", Principal: "pony:pony", CreatedBy: "ranch", PolicyPush: "open"}); err != nil {
		t.Fatal(err)
	}
	d.grant(t, "pony:pony", "pony@pony.example.com", "bcdf-correct-horse", "git:pony:fetch", "git:pony:push")
	d.grant(t, "member:reader", "reader@pony.example.com", "bcdf-only-looking-here", "git:pony:fetch")
	d.grant(t, "member:elsewhere", "elsewhere@pony.example.com", "bcdf-another-door-key", "git:other:push", "drive:*:*")

	// No credentials: a challenge, not a leak.
	res, err := http.Get(d.srv.URL + "/git/pony.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anonymous: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	// A repository that does not exist, anonymous: the same challenge.
	res, _ = http.Get(d.srv.URL + "/git/nothere.git/info/refs?service=git-upload-pack")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous on a missing repository: %d, want 401 (nothing learned)", res.StatusCode)
	}

	work := t.TempDir()
	// The owner's credential clones and pushes.
	owner := d.authURL("pony", "pony@pony.example.com", "bcdf-correct-horse")
	d.must(work, "clone", owner, "a")
	a := filepath.Join(work, "a")
	write(t, a, "config/MISSION.md", "# Mission\n")
	d.must(a, "add", ".")
	d.must(a, "commit", "-qm", "first")
	d.must(a, "push", "-q", "origin", "main")

	// A fetch-only credential clones but cannot push.
	reader := d.authURL("pony", "reader@pony.example.com", "bcdf-only-looking-here")
	d.must(work, "clone", "-q", reader, "r")
	r := filepath.Join(work, "r")
	write(t, r, "config/AGENTS.md", "Be brief.\n")
	d.must(r, "add", ".")
	d.must(r, "commit", "-qm", "reader tries")
	d.fails(r, "Authentication failed", "push", "origin", "main")

	// The right password for another door: refused like a wrong one.
	d.fails(work, "Authentication failed", "clone", d.authURL("pony", "elsewhere@pony.example.com", "bcdf-another-door-key"), "e")
	// A wrong password.
	d.fails(work, "Authentication failed", "clone", d.authURL("pony", "pony@pony.example.com", "bcdf-not-the-password"), "w")
	// A repository the tenant does not have: a credential scoped to another
	// repository learns nothing (401, the scope is the door); one scoped to
	// every repository of the tenant (`git:*:fetch`) is told 404.
	res, _ = http.Get(d.authURL("nothere", "pony@pony.example.com", "bcdf-correct-horse") + "/info/refs?service=git-upload-pack")
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a missing repository with another repository's credential: %d, want 401", res.StatusCode)
	}
	d.grant(t, "member:admin", "admin@pony.example.com", "bcdf-sees-every-repo", "git:*:fetch")
	res, _ = http.Get(d.authURL("nothere", "admin@pony.example.com", "bcdf-sees-every-repo") + "/info/refs?service=git-upload-pack")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing repository with a tenant-wide credential: %d, want 404", res.StatusCode)
	}
	d.must(work, "clone", "-q", d.authURL("pony", "admin@pony.example.com", "bcdf-sees-every-repo"), "adm")

	// What was pushed is in the durable store: a fresh loader sees it.
	repo, found, err := d.repos.Open(ctx, "acme", "pony")
	if err != nil || !found {
		t.Fatal(err)
	}
	refs, _ := repo.Refs(ctx)
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs after the push: %+v", refs)
	}
	log, err := d.repos.Store().RefLog(ctx, repo.Meta().ID, 10)
	if err != nil || len(log) != 1 || log[0].Principal != "pony:pony" || log[0].Source != "push" || log[0].Credential == "" {
		t.Fatalf("the push's audit row: %+v %v", log, err)
	}
}

func TestDoorClosedWithoutResolver(t *testing.T) {
	h := newHarness(t, defaultLimits)
	h.ctrl.openWithoutAuth = false
	res, err := http.Get(h.srv.URL + "/git/pony.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no resolver: %d, want 503", res.StatusCode)
	}
}

func TestDoorRequiresTLSUnlessInsecure(t *testing.T) {
	h := newHarness(t, defaultLimits)
	h.ctrl.insecureAuth = false
	res, err := http.Get(h.srv.URL + "/git/pony.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("plaintext without --git-insecure-auth: %d, want 403", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/git/pony.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("behind a TLS-terminating proxy: %d, want 200", res.StatusCode)
	}
}
