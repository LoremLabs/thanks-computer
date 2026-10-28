package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	gitcore "github.com/loremlabs/thanks-computer/chassis/git"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	gitp "github.com/loremlabs/thanks-computer/chassis/server/personality/git"
)

func newRepoDeps(t *testing.T) repoDeps {
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
	fixed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	return repoDeps{
		repos: gitcore.NewStoreRepos(st, fs, gitcore.Limits{MaxObjectBytes: 1 << 20, MaxObjects: 5000, MaxRepoBytes: 64 << 20, TextOnly: true}),
		now:   func() time.Time { return fixed },
	}
}

// callRepo runs an op as a rule of `stack` in `tenant`.
func callRepo(t *testing.T, fn func(context.Context, repoDeps, []byte) (event.Payload, error), d repoDeps, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"demo/100/repo"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func noErr(t *testing.T, out string) {
	t.Helper()
	if e := gjson.Get(out, "_repo.error"); e.Exists() {
		t.Fatalf("op error: %s", e.Raw)
	}
}

type repoFakeResolver map[string]string

func (f repoFakeResolver) ResolveErr(k ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	tnt, ok := f[k.Hostname]
	return ingress.RouteTarget{Tenant: tnt, Stack: "core", Verified: true}, ok, nil
}

func TestRepoOps(t *testing.T) {
	d := newRepoDeps(t)

	// Disabled store, no tenant.
	out := callRepo(t, repoCreate, repoDeps{}, "acme", "ranch", `{"name":"pony"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_disabled" {
		t.Fatalf("disabled: %s", out)
	}
	out = callRepo(t, repoCreate, d, "", "ranch", `{"name":"pony"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_no_tenant" {
		t.Fatalf("no tenant: %s", out)
	}

	// Create, idempotently; the quotas and policy land.
	out = callRepo(t, repoCreate, d, "acme", "ranch", `{"name":"pony","principal":"pony:pony","policy":{"push":"stack"},"quotas":{"max_repo_bytes":2097152}}`)
	noErr(t, out)
	if !gjson.Get(out, "_repo.created").Bool() || gjson.Get(out, "_repo.head").String() != "refs/heads/main" || gjson.Get(out, "_repo.created_by").String() != "ranch" {
		t.Fatalf("create: %s", out)
	}
	out = callRepo(t, repoCreate, d, "acme", "loop", `{"name":"pony","principal":"someone:else"}`)
	noErr(t, out)
	if gjson.Get(out, "_repo.created").Bool() || gjson.Get(out, "_repo.principal").String() != "pony:pony" {
		t.Fatalf("second create: %s", out)
	}
	out = callRepo(t, repoCreate, d, "acme", "ranch", `{"name":"bad/name"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_invalid_arg" {
		t.Fatalf("bad name: %s", out)
	}

	// Empty: refs answer, read says not found.
	out = callRepo(t, repoRefs, d, "acme", "loop", `{"name":"pony"}`)
	noErr(t, out)
	if gjson.Get(out, "_repo.main").String() != "" || len(gjson.Get(out, "_repo.refs").Array()) != 0 {
		t.Fatalf("empty refs: %s", out)
	}
	out = callRepo(t, repoRead, d, "acme", "loop", `{"name":"pony","ref":"active"}`)
	noErr(t, out)
	if gjson.Get(out, "_repo.found").Bool() {
		t.Fatalf("read of an empty repository: %s", out)
	}

	// The first commit: no parent, two files in a folder and one at the root.
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"","message":"Hello, pony",
		"author":{"name":"Pony","email":"pony@pony.example.com"},
		"writes":[{"path":"config/MISSION.md","content":"# Mission\n"},{"path":"config/AGENTS.md","content":"<!-- standing direction -->\n"},{"path":"README.md","content":"hello\n"}]}`)
	noErr(t, out)
	first := gjson.Get(out, "_repo.commit").String()
	if len(first) != 40 || gjson.Get(out, "_repo.old").String() != gitstore.ZeroOID || gjson.Get(out, "_repo.changed").Int() != 3 {
		t.Fatalf("first commit: %s", out)
	}
	// active → that commit, by the application.
	out = callRepo(t, repoRefUpdate, d, "acme", "loop", `{"name":"pony","ref":"refs/txco/active","old":"","new":"`+first+`"}`)
	noErr(t, out)
	out = callRepo(t, repoRefs, d, "acme", "loop", `{"name":"pony"}`)
	if gjson.Get(out, "_repo.main").String() != first || gjson.Get(out, "_repo.active").String() != first {
		t.Fatalf("refs after the seed: %s", out)
	}
	// Read the config/ tree at active.
	out = callRepo(t, repoRead, d, "acme", "loop", `{"name":"pony","ref":"active","prefix":"config/"}`)
	noErr(t, out)
	files := gjson.Get(out, "_repo.files").Array()
	if !gjson.Get(out, "_repo.found").Bool() || len(files) != 2 || files[0].Get("path").String() != "config/AGENTS.md" || files[1].Get("content").String() != "# Mission\n" {
		t.Fatalf("read: %s", out)
	}
	// Nothing to change: no commit.
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"`+first+`","message":"noop","writes":[{"path":"README.md","content":"hello\n"}]}`)
	noErr(t, out)
	if !gjson.Get(out, "_repo.unchanged").Bool() || gjson.Get(out, "_repo.commit").String() != first {
		t.Fatalf("noop commit: %s", out)
	}
	// A stale expected: moved, nothing written.
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"","message":"stale","writes":[{"path":"x.md","content":"x\n"}]}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_moved" || gjson.Get(out, "_repo.current").String() != first {
		t.Fatalf("stale: %s", out)
	}
	// The second commit: a change, a delete.
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"`+first+`","message":"Refunds policy",
		"writes":[{"path":"config/policies/refunds.md","content":"Check the purchase date.\n"}],"deletes":["README.md"]}`)
	noErr(t, out)
	second := gjson.Get(out, "_repo.commit").String()
	if gjson.Get(out, "_repo.old").String() != first || gjson.Get(out, "_repo.changed").Int() != 2 {
		t.Fatalf("second commit: %s", out)
	}
	// Log, diff.
	out = callRepo(t, repoLog, d, "acme", "loop", `{"name":"pony","ref":"main","limit":10}`)
	noErr(t, out)
	commits := gjson.Get(out, "_repo.commits").Array()
	if len(commits) != 2 || commits[0].Get("commit").String() != second || commits[1].Get("message").String() != "Hello, pony" || commits[0].Get("parents.0").String() != first {
		t.Fatalf("log: %s", out)
	}
	out = callRepo(t, repoDiff, d, "acme", "loop", `{"name":"pony","base":"`+first+`","head":"`+second+`"}`)
	noErr(t, out)
	changes := gjson.Get(out, "_repo.changes").Array()
	if len(changes) != 2 || changes[0].Get("path").String() != "README.md" || changes[0].Get("op").String() != "delete" ||
		changes[1].Get("op").String() != "add" || !strings.Contains(changes[1].Get("patch").String(), "+Check the purchase date.") {
		t.Fatalf("diff: %s", out)
	}
	// A proposal: a new ref from a base.
	out = callRepo(t, repoCommit, d, "acme", "loop", `{"name":"pony","ref":"refs/heads/propose/hld_1","expected":"","base":"active","message":"Proposed: be brief",
		"writes":[{"path":"config/AGENTS.md","content":"Be brief.\n"}]}`)
	noErr(t, out)
	proposed := gjson.Get(out, "_repo.commit").String()
	out = callRepo(t, repoLog, d, "acme", "loop", `{"name":"pony","commit":"`+proposed+`","limit":5}`)
	if cs := gjson.Get(out, "_repo.commits").Array(); len(cs) != 2 || cs[1].Get("commit").String() != first {
		t.Fatalf("the proposal's parent should be active (%s): %s", first, out)
	}
	// A rollback: main's tree becomes the first commit's, as a new commit.
	out = callRepo(t, repoCommit, d, "acme", "loop", `{"name":"pony","ref":"main","expected":"`+second+`","tree":"`+first+`","message":"Restore"}`)
	noErr(t, out)
	restored := gjson.Get(out, "_repo.commit").String()
	out = callRepo(t, repoRead, d, "acme", "loop", `{"name":"pony","commit":"`+restored+`"}`)
	if fs := gjson.Get(out, "_repo.files").Array(); len(fs) != 3 || fs[0].Get("path").String() != "README.md" {
		t.Fatalf("restored tree: %s", out)
	}
	// Ref update guards: a moved old, a missing object, the default branch's deletion.
	out = callRepo(t, repoRefUpdate, d, "acme", "loop", `{"name":"pony","ref":"active","old":"`+second+`","new":"`+restored+`"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_moved" || gjson.Get(out, "_repo.current").String() != first {
		t.Fatalf("moved active: %s", out)
	}
	out = callRepo(t, repoRefUpdate, d, "acme", "loop", `{"name":"pony","ref":"active","old":"`+first+`","new":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_invalid_arg" {
		t.Fatalf("unknown object: %s", out)
	}
	out = callRepo(t, repoRefUpdate, d, "acme", "loop", `{"name":"pony","ref":"main","old":"`+restored+`","new":""}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_invalid_arg" {
		t.Fatalf("delete main: %s", out)
	}
	// Another tenant sees nothing.
	out = callRepo(t, repoRefs, d, "other", "loop", `{"name":"pony"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_not_found" {
		t.Fatalf("other tenant: %s", out)
	}
	// Caps: a write over the object cap, a binary.
	big := strings.Repeat("x", 1<<20+1)
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"`+restored+`","message":"big","writes":[{"path":"big.md","content":"`+big+`"}]}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_too_large" {
		t.Fatalf("over the cap: %s", gjson.Get(out, "_repo.error"))
	}
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"pony","ref":"main","expected":"`+restored+`","message":"bin","writes":[{"path":"bin.md","content":"a\u0000b"}]}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_invalid_arg" {
		t.Fatalf("binary: %s", gjson.Get(out, "_repo.error"))
	}

	// What the ops wrote is a repository git itself accepts: clone it
	// through the head and fsck.
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	conf := config.Config{Personalities: "web,git", GitPathPrefix: "/git", GitInsecureAuth: true, LoginRate: 100,
		GitMaxPackBytes: 16 << 20, GitMaxObjectBytes: 1 << 20, GitMaxObjects: 5000, GitMaxRepoBytes: 64 << 20, GitTextOnly: true,
		GitRespTimeout: "10s", GitObserveSample: 1, GitObserveMaxInflight: 2}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := gitp.NewController(ctx, pu, d.repos, repoFakeResolver{"127.0.0.1": "acme"})
	ids := authntest.NewSQLiteStore(t)
	ctrl.SetAuth(authn.NewResolver(ids, authn.ResolverConfig{Rate: 100, TenantID: authntest.SameTenantID}))
	p, _ := authn.ParsePrincipal("pony:pony")
	authntest.Grant(t, ids, "acme", p, "pony@pony.example.com", "bcdf-correct-horse", "git:pony:fetch")
	ctrl.Start()
	srv := httptest.NewServer(ctrl.Handler())
	defer func() { srv.Close(); ctrl.Stop() }()
	u, _ := url.Parse(srv.URL)
	u.User = url.UserPassword("pony@pony.example.com", "bcdf-correct-horse")
	work := t.TempDir()
	run := func(dir string, args ...string) string {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0", "HOME="+work)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run(work, "clone", "-q", u.String()+"/git/pony.git", "c")
	c := filepath.Join(work, "c")
	run(c, "fsck", "--strict")
	if got := strings.TrimSpace(run(c, "rev-list", "--count", "HEAD")); got != "3" {
		t.Fatalf("commits in the clone: %s, want 3", got)
	}
	if b, err := os.ReadFile(filepath.Join(c, "README.md")); err != nil || string(b) != "hello\n" {
		t.Fatalf("the restored README: %v %q", err, b)
	}
	run(c, "fetch", "-q", "origin", "refs/txco/active:refs/remotes/origin/active", "refs/heads/propose/hld_1:refs/remotes/origin/propose/hld_1")
	if got := strings.TrimSpace(run(c, "rev-parse", "origin/active")); got != first {
		t.Fatalf("active from a client: %s, want %s", got, first)
	}

	// Remove: another stack may not; the creator may; twice is fine.
	out = callRepo(t, repoRemove, d, "acme", "loop", `{"name":"pony"}`)
	if gjson.Get(out, "_repo.error.code").String() != "txco_repo_forbidden" {
		t.Fatalf("remove by another stack: %s", out)
	}
	out = callRepo(t, repoRemove, d, "acme", "ranch", `{"name":"pony"}`)
	noErr(t, out)
	if !gjson.Get(out, "_repo.removed").Bool() {
		t.Fatalf("remove: %s", out)
	}
	out = callRepo(t, repoRemove, d, "acme", "ranch", `{"name":"pony"}`)
	noErr(t, out)
	if gjson.Get(out, "_repo.removed").Bool() {
		t.Fatalf("second remove: %s", out)
	}
}

func TestWriteTreeOrder(t *testing.T) {
	// git orders "a-b" (a file) before "a" (a directory, compared as "a/"):
	// '-' (0x2d) sorts before '/' (0x2f).
	d := newRepoDeps(t)
	out := callRepo(t, repoCreate, d, "acme", "ranch", `{"name":"order"}`)
	noErr(t, out)
	out = callRepo(t, repoCommit, d, "acme", "ranch", `{"name":"order","ref":"main","message":"order",
		"writes":[{"path":"a/x.md","content":"x\n"},{"path":"a-b","content":"y\n"},{"path":"a.md","content":"z\n"},{"path":"b/c/d.md","content":"d\n"}]}`)
	noErr(t, out)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	repo, _, _ := d.repos.Open(context.Background(), "acme", "order")
	refs, _ := repo.Refs(context.Background())
	// Rebuild the root tree bytes with git's own hashing: the ids must agree.
	work := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "HOME="+work)
	run := func(args ...string) string {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = work
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	for _, f := range []struct{ p, c string }{{"a/x.md", "x\n"}, {"a-b", "y\n"}, {"a.md", "z\n"}, {"b/c/d.md", "d\n"}} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(work, f.p)), 0o755)
		_ = os.WriteFile(filepath.Join(work, f.p), []byte(f.c), 0o644)
	}
	run("add", ".")
	wantTree := run("write-tree")
	out = callRepo(t, repoRead, d, "acme", "ranch", `{"name":"order","commit":"`+refs[0].Hash.String()+`"}`)
	if got := gjson.Get(out, "_repo.tree").String(); got != wantTree {
		t.Fatalf("root tree id %s, git says %s", got, wantTree)
	}
}
