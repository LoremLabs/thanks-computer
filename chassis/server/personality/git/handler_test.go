package git

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

type fakeResolver map[string]string

func (f fakeResolver) ResolveErr(k ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	t, ok := f[k.Hostname]
	return ingress.RouteTarget{Tenant: t, Stack: "core", Verified: true}, ok, nil
}

var defaultLimits = Limits{MaxPackBytes: 16 << 20, MaxObjectBytes: 1 << 20, MaxObjects: 5000, MaxRepoBytes: 64 << 20, TextOnly: true}

type harness struct {
	t    *testing.T
	ctrl *Controller
	srv  *httptest.Server
	home string // a HOME of its own, so the developer's git config stays out
}

func newHarness(t *testing.T, limits Limits) *harness {
	t.Helper()
	conf := config.Config{
		Personalities: "web,git", GitPathPrefix: "/git", GitInsecureAuth: true,
		GitMaxPackBytes: int(limits.MaxPackBytes), GitMaxObjectBytes: int(limits.MaxObjectBytes),
		GitMaxObjects: limits.MaxObjects, GitMaxRepoBytes: int(limits.MaxRepoBytes), GitTextOnly: limits.TextOnly,
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, NewMemRepos(limits), fakeResolver{"127.0.0.1": "acme"})
	ctrl.Start()
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(func() { srv.Close(); ctrl.Stop(); cancel() })
	return &harness{t: t, ctrl: ctrl, srv: srv, home: t.TempDir()}
}

func (h *harness) url(repo string) string { return h.srv.URL + "/git/" + repo + ".git" }

// gitPath is the client under test; without one the suite skips.
func gitPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	return p
}

// git runs the client in dir and returns its combined output and error.
func (h *harness) git(dir string, env []string, args ...string) (string, error) {
	h.t.Helper()
	base := []string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main",
		"-c", "commit.gpgsign=false", "-c", "advice.detachedHead=false"}
	cmd := exec.Command(gitPath(h.t), append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+h.home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/echo", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// must runs git and fails the test when it does.
func (h *harness) must(dir string, args ...string) string {
	h.t.Helper()
	out, err := h.git(dir, nil, args...)
	if err != nil {
		h.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// fails runs git and fails the test unless it fails with want in its output.
func (h *harness) fails(dir string, want string, args ...string) string {
	h.t.Helper()
	out, err := h.git(dir, nil, args...)
	if err == nil {
		h.t.Fatalf("git %s succeeded; wanted a failure mentioning %q\n%s", strings.Join(args, " "), want, out)
	}
	if !strings.Contains(out, want) {
		h.t.Fatalf("git %s failed without %q:\n%s", strings.Join(args, " "), want, out)
	}
	return out
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seeded clones the empty repository into <work>/a, pushes a first commit
// and clones it again into <work>/b.
func (h *harness) seeded(repo string) (a, b string) {
	h.t.Helper()
	work := h.t.TempDir()
	out := h.must(work, "clone", h.url(repo), "a")
	if !strings.Contains(out, "empty repository") {
		h.t.Fatalf("cloning an empty repository should warn; got:\n%s", out)
	}
	a = filepath.Join(work, "a")
	write(h.t, a, "config/MISSION.md", "# Mission\n\nAnswer kindly.\n")
	write(h.t, a, "README.md", "hello\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "first")
	h.must(a, "push", "-q", "origin", "main")
	h.must(work, "clone", "-q", h.url(repo), "b")
	b = filepath.Join(work, "b")
	return a, b
}

func TestCloneEmptyPushClone(t *testing.T) {
	h := newHarness(t, defaultLimits)
	_, b := h.seeded("pony")
	if got := strings.TrimSpace(h.must(b, "symbolic-ref", "HEAD")); got != "refs/heads/main" {
		t.Fatalf("HEAD = %q, want refs/heads/main", got)
	}
	body, err := os.ReadFile(filepath.Join(b, "config", "MISSION.md"))
	if err != nil || !strings.Contains(string(body), "Answer kindly") {
		t.Fatalf("the clone lacks the pushed file: %v %q", err, body)
	}
	if got := strings.TrimSpace(h.must(b, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("commits = %s, want 1", got)
	}
}

func TestIncrementalFetchUsesCommons(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, b := h.seeded("pony")
	write(t, a, "config/AGENTS.md", "Be brief.\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "second")
	h.must(a, "push", "-q", "origin", "main") // a thin pack: deltas against the first push
	trace := filepath.Join(t.TempDir(), "packet.trace")
	if out, err := h.git(b, []string{"GIT_TRACE_PACKET=" + trace}, "fetch", "-q", "origin"); err != nil {
		t.Fatalf("fetch: %v\n%s", err, out)
	}
	tr, _ := os.ReadFile(trace)
	if !strings.Contains(string(tr), "ACK") || !strings.Contains(string(tr), "common") {
		t.Fatalf("the negotiation did not acknowledge a common commit:\n%s", tr)
	}
	if got := strings.TrimSpace(h.must(b, "rev-list", "--count", "origin/main")); got != "2" {
		t.Fatalf("origin/main commits = %s, want 2", got)
	}
	h.must(b, "pull", "-q", "--ff-only", "origin", "main")
	if _, err := os.Stat(filepath.Join(b, "config", "AGENTS.md")); err != nil {
		t.Fatalf("pull did not bring the second commit: %v", err)
	}
}

func TestNonFastForwardThenRebase(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, b := h.seeded("pony")
	write(t, a, "config/AGENTS.md", "From a.\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "a2")
	h.must(a, "push", "-q", "origin", "main")
	write(t, b, "config/VOICE.md", "From b.\n")
	h.must(b, "add", ".")
	h.must(b, "commit", "-qm", "b2")
	h.fails(b, "[rejected]", "push", "origin", "main")
	h.must(b, "pull", "-q", "--rebase", "origin", "main")
	h.must(b, "push", "-q", "origin", "main")
	if got := strings.TrimSpace(h.must(a, "ls-remote", "--heads", "origin", "main")); !strings.Contains(got, "refs/heads/main") {
		t.Fatalf("ls-remote: %q", got)
	}
}

func TestRefusals(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, _ := h.seeded("pony")
	h.fails(a, "refs/txco/* is read-only", "push", "origin", "main:refs/txco/active")
	h.must(a, "tag", "v1")
	h.fails(a, "only branches", "push", "origin", "v1")
	h.fails(a, "default branch cannot be deleted", "push", "origin", "--delete", "main")
	// A branch at an existing commit: a zero-object pack.
	h.must(a, "push", "-q", "origin", "main:refs/heads/propose/x")
	if got := h.must(a, "ls-remote", "--heads", "origin"); !strings.Contains(got, "refs/heads/propose/x") {
		t.Fatalf("propose/x not created:\n%s", got)
	}
	// And its deletion: no pack at all.
	h.must(a, "push", "-q", "origin", "--delete", "propose/x")
	if got := h.must(a, "ls-remote", "--heads", "origin"); strings.Contains(got, "propose/x") {
		t.Fatalf("propose/x not deleted:\n%s", got)
	}
}

func TestAtomicAllOrNothing(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, _ := h.seeded("pony")
	before := strings.Fields(h.must(a, "ls-remote", "--heads", "origin", "main"))[0]
	write(t, a, "config/AGENTS.md", "Second.\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "second")
	out := h.fails(a, reasonAtomic, "push", "origin", "main", "main:refs/txco/x")
	if !strings.Contains(out, "refs/txco/* is read-only") {
		t.Fatalf("the refused ref's reason is missing:\n%s", out)
	}
	after := strings.Fields(h.must(a, "ls-remote", "--heads", "origin", "main"))[0]
	if before != after {
		t.Fatalf("main moved from %s to %s despite the refused sibling", before, after)
	}
}

func TestLargePushProbeAndCap(t *testing.T) {
	h := newHarness(t, Limits{MaxPackBytes: 16 << 20, MaxObjectBytes: 8 << 20, MaxObjects: 5000, TextOnly: true})
	a, _ := h.seeded("pony")
	// 2 MiB of incompressible text: over http.postBuffer, so the client
	// probes with a flush, then streams the request chunked.
	raw := make([]byte, 1536<<10)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	write(t, a, "config/knowledge/big.md", base64.StdEncoding.EncodeToString(raw)+"\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "big")
	h.must(a, "push", "-q", "origin", "main")

	small := newHarness(t, Limits{MaxPackBytes: 1 << 20, MaxObjectBytes: 8 << 20, MaxObjects: 5000, TextOnly: true})
	h.must(a, "remote", "add", "small", small.url("pony"))
	h.fails(a, "too large", "push", "small", "main")

	tiny := newHarness(t, Limits{MaxPackBytes: 16 << 20, MaxObjectBytes: 4096, MaxObjects: 5000, TextOnly: true})
	h.must(a, "remote", "add", "tiny", tiny.url("pony"))
	h.fails(a, "over 4096 bytes", "push", "tiny", "main")
}

func TestTextOnly(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, _ := h.seeded("pony")
	write(t, a, "config/bin.md", "ab\x00cd\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "binary")
	h.fails(a, "is not text", "push", "origin", "main")
	h.must(a, "reset", "-q", "--hard", "HEAD~1")

	if err := os.Symlink("MISSION.md", filepath.Join(a, "config", "link.md")); err != nil {
		t.Skip("no symlinks here")
	}
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "symlink")
	h.fails(a, "is a symlink", "push", "origin", "main")
	h.must(a, "reset", "-q", "--hard", "HEAD~1")

	write(t, a, "config/run.sh", "#!/bin/sh\necho hi\n")
	h.must(a, "add", "--chmod=+x", "config/run.sh")
	h.must(a, "commit", "-qm", "exec")
	h.fails(a, "is executable", "push", "origin", "main")
}

func TestHookRunsAfterHeadersAndCanRefuse(t *testing.T) {
	h := newHarness(t, defaultLimits)
	a, _ := h.seeded("pony")
	// The hook is set once (a request's goroutine may outlive the client's
	// reply); what it does is swapped through an atomic.
	var hook atomic.Pointer[func(*Push) error]
	h.ctrl.beforeRefs = func(_ context.Context, p *Push) error {
		if f := hook.Load(); f != nil {
			return (*f)(p)
		}
		return nil
	}
	seen := make(chan string, 8)
	observe := func(p *Push) error {
		time.Sleep(1200 * time.Millisecond) // the lane's wait: headers are already out
		for _, chs := range p.Changes {
			for _, ch := range chs {
				seen <- ch.Op + " " + ch.Path
			}
		}
		return nil
	}
	hook.Store(&observe)
	write(t, a, "config/AGENTS.md", "Be brief.\n")
	h.must(a, "add", ".")
	h.must(a, "commit", "-qm", "second")
	h.must(a, "push", "-q", "origin", "main")
	if got := <-seen; got != "add config/AGENTS.md" {
		t.Fatalf("change seen by the hook: %q", got)
	}
	decline := func(*Push) error { return errors.New("declined by the stack: config/MISSION.md must stay") }
	hook.Store(&decline)
	h.must(a, "rm", "-q", "config/MISSION.md")
	h.must(a, "commit", "-qm", "remove mission")
	h.fails(a, "declined by the stack: config/MISSION.md must stay", "push", "origin", "main")
	if got := strings.TrimSpace(h.must(a, "rev-list", "--count", "origin/main")); got != "2" {
		t.Fatalf("origin/main moved despite the refusal: %s commits", got)
	}
}

func TestProtocolV0Client(t *testing.T) {
	h := newHarness(t, defaultLimits)
	_, b := h.seeded("pony")
	// Every call above runs with the client's default (v2, falling back);
	// a client pinned to v0 must work the same.
	work := t.TempDir()
	h.must(work, "-c", "protocol.version=0", "clone", "-q", h.url("pony"), "c")
	if _, err := os.Stat(filepath.Join(work, "c", "README.md")); err != nil {
		t.Fatal(err)
	}
	_ = b
}

func TestRouteAndCommandChecks(t *testing.T) {
	c := &Controller{prefix: "/git"}
	for _, tc := range []struct {
		path          string
		name, service string
		ok            bool
	}{
		{"/git/pony.git/info/refs", "pony", "info/refs", true},
		{"/git/pony/git-upload-pack", "pony", uploadPackService, true},
		{"/git/a.b-c_d.git/git-receive-pack", "a.b-c_d", receivePackService, true},
		{"/git/pony.git/HEAD", "", "", false},
		{"/git/pony.git/objects/info/packs", "", "", false},
		{"/git/../x.git/info/refs", "", "", false},
		{"/drive/pony.git/info/refs", "", "", false},
		{"/git", "", "", false},
	} {
		name, service, ok := c.route(tc.path)
		if name != tc.name || service != tc.service || ok != tc.ok {
			t.Errorf("route(%q) = %q %q %v, want %q %q %v", tc.path, name, service, ok, tc.name, tc.service, tc.ok)
		}
	}
	req, err := readUploadRequest(strings.NewReader(
		"0064want 1111111111111111111111111111111111111111 multi_ack_detailed side-band-64k agent=git/2.50.1\n" +
			"0032want 2222222222222222222222222222222222222222\n0000" +
			"0032have 3333333333333333333333333333333333333333\n0009done\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.wants) != 2 || len(req.haves) != 1 || !req.done || !req.caps.Supports("multi_ack_detailed") {
		t.Fatalf("upload request misread: %+v", req)
	}
	if _, err := readUploadRequest(strings.NewReader("000ddeepen 1\n0000")); err == nil {
		t.Fatal("deepen should be refused")
	}
}
