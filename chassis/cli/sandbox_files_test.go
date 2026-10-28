package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
)

const testKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n"

// fakeGrantChassis answers opens as the gateway would: the variables, one
// file, or the list; it counts what it was asked and can be told to refuse.
type fakeGrantChassis struct {
	mu      sync.Mutex
	token   string // GH_TOKEN's value now
	refuse  bool   // refuse every open, the list included
	asks    []grantwire.Request
	serve   []string
	release int
}

func (f *fakeGrantChassis) open(_ context.Context, req grantwire.Request) (grantwire.Opened, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asks = append(f.asks, req)
	if f.refuse {
		return grantwire.Opened{}, grantwire.ErrRefused
	}
	switch req.Entry {
	case grantwire.EntryList:
		return grantwire.Opened{Serve: f.serve}, nil
	case "file:~/.ssh/id_ed25519":
		f.release++
		return grantwire.Opened{Env: map[string][]byte{req.Entry: []byte(testKey)}}, nil
	default:
		f.release++
		return grantwire.Opened{Env: map[string][]byte{"GH_TOKEN": []byte(f.token), "NOTE": []byte("it's here")}}, nil
	}
}

func (f *fakeGrantChassis) seen() (int, []grantwire.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.release, append([]grantwire.Request(nil), f.asks...)
}

func (f *fakeGrantChassis) set(token string, refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token, f.refuse = token, refuse
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func readWithin(t *testing.T, path string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		b, err := os.ReadFile(path)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		got <- string(b)
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatalf("reading %s: nothing came", path)
		return ""
	}
}

func fastPoll(t *testing.T) {
	old := sandboxPoll
	sandboxPoll = 5 * time.Millisecond
	t.Cleanup(func() { sandboxPoll = old })
}

// TestFilesAreServedFreshAtEachRead: a pipe per file, mode 0600; each read
// asks the chassis then, so a value that changed between reads comes back
// changed; a multi-line key comes back byte for byte; the log names each
// delivery and never a value; stopping removes the pipes.
func TestFilesAreServedFreshAtEachRead(t *testing.T) {
	fastPoll(t)
	dir, home := t.TempDir(), t.TempDir()
	fc := &fakeGrantChassis{token: "ghu_first"}
	logs := &syncBuf{}
	srv, err := newFileServer(fc.open, "rg1.a.b", logs, dir, home,
		map[string][]string{"github": {"dotenv:run.env", "file:~/.ssh/id_ed25519"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.prepare(); err != nil {
		t.Fatal(err)
	}
	envPath, keyPath := filepath.Join(dir, "run.env"), filepath.Join(home, ".ssh", "id_ed25519")
	for _, p := range []string{envPath, keyPath} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeNamedPipe == 0 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi.Mode(), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.serve(ctx)

	if got := readWithin(t, envPath); got != "GH_TOKEN=ghu_first\nNOTE='it'\\''s here'\n" {
		t.Errorf("first read: %q", got)
	}
	fc.set("ghu_renewed", false)
	if got := readWithin(t, envPath); got != "GH_TOKEN=ghu_renewed\nNOTE='it'\\''s here'\n" {
		t.Errorf("second read, after the value changed: %q", got)
	}
	if got := readWithin(t, keyPath); got != testKey {
		t.Errorf("the key: %q", got)
	}
	if got := readWithin(t, keyPath); got != testKey {
		t.Errorf("the key again: %q", got)
	}
	cancel()
	srv.stop()
	for _, p := range []string{envPath, keyPath, envPath + ".next"} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still there after stop: %v", p, err)
		}
	}
	n, asks := fc.seen()
	if n != 4 {
		t.Errorf("%d releases, want one per read", n)
	}
	for _, a := range asks {
		if a.Token != "rg1.a.b" || a.Sandbox != "github" {
			t.Errorf("asked %+v", a)
		}
	}
	out := logs.String()
	if strings.Count(out, "delivered") != 4 || strings.Contains(out, "ghu_") || strings.Contains(out, "AAAA") {
		t.Errorf("log: %s", out)
	}
}

// TestARefusalEndsServingWhenTheGrantIsGone: a read the chassis refuses gets
// nothing; when the chassis then refuses even to list the files, the run's
// grant is over and serving stops by itself.
func TestARefusalEndsServingWhenTheGrantIsGone(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	fc := &fakeGrantChassis{token: "ghu", refuse: true}
	srv, err := newFileServer(fc.open, "t", io.Discard, dir, "", map[string][]string{"github": {"dotenv:run.env"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.serve(ctx)
	if got := readWithin(t, filepath.Join(dir, "run.env")); got != "" {
		t.Errorf("a refused read: %q", got)
	}
	select {
	case <-srv.Gone():
	case <-time.After(5 * time.Second):
		t.Fatal("serving did not stop when the grant was gone")
	}
	cancel()
	srv.stop()
}

// TestPrepareNeverTouchesAFile: a regular file at a served path is left as
// it is; a pipe a dead server left is replaced; a path outside the rules,
// or two sandboxes serving one path, is refused.
func TestPrepareNeverTouchesAFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "run.env")
	if err := os.WriteFile(real, []byte("MINE=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, _ := newFileServer((&fakeGrantChassis{}).open, "t", io.Discard, dir, "", map[string][]string{"github": {"dotenv:run.env"}})
	if err := srv.prepare(); err == nil || !strings.Contains(err.Error(), "not a pipe") {
		t.Errorf("prepare over a file: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != "MINE=1\n" {
		t.Errorf("the file was touched: %q", b)
	}
	stale := filepath.Join(dir, "stale.env")
	if err := mkfifo(stale); err != nil {
		t.Fatal(err)
	}
	srv, _ = newFileServer((&fakeGrantChassis{}).open, "t", io.Discard, dir, "", map[string][]string{"github": {"dotenv:stale.env"}})
	if err := srv.prepare(); err != nil {
		t.Errorf("over a stale pipe: %v", err)
	}
	srv.remove()
	if _, err := newFileServer((&fakeGrantChassis{}).open, "t", io.Discard, dir, "", map[string][]string{"a": {"file:x"}, "b": {"file:x"}}); err == nil {
		t.Error("two sandboxes serving one path")
	}
	if _, err := newFileServer((&fakeGrantChassis{}).open, "t", io.Discard, dir, "", map[string][]string{"a": {"file:../x"}}); err == nil {
		t.Error("a path that walks up")
	}
	if _, err := newFileServer((&fakeGrantChassis{}).open, "t", io.Discard, dir, "", map[string][]string{"a": {"file:~/x"}}); err == nil {
		t.Error("~/ with no home")
	}
}

// TestTheExecFormServesFilesWhileTheProgramRuns: a sandbox with files makes
// `txco sandbox NAME -- PROGRAM` the program's parent: the files are there
// while it runs and gone after, and its exit code is the command's.
func TestTheExecFormServesFilesWhileTheProgramRuns(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	r := newSandboxRig(t, append(grantEnviron, "HOME="+dir)...)
	fc := &fakeGrantChassis{token: "ghu_x", serve: []string{"dotenv:run.env"}}
	sandboxOpen = func(ctx context.Context, socket string, req grantwire.Request) (grantwire.Opened, error) {
		o, err := fc.open(ctx, req)
		if req.Entry == "" && err == nil {
			o.Serve = fc.serve
		}
		return o, err
	}
	var read string
	oldStart := sandboxStart
	t.Cleanup(func() { sandboxStart = oldStart })
	sandboxStart = func(path string, argv, env []string, stderr io.Writer) int {
		read = readWithin(t, filepath.Join(dir, "run.env"))
		return 3
	}
	code, _, stderr := runSandboxIn("--dir", dir, "github", "--", "git", "push")
	if code != 3 || r.started {
		t.Fatalf("exit %d (want the program's 3), replaced=%v, stderr %q", code, r.started, stderr)
	}
	if read != "GH_TOKEN=ghu_x\nNOTE='it'\\''s here'\n" {
		t.Errorf("the program read %q", read)
	}
	if _, err := os.Lstat(filepath.Join(dir, "run.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file outlived the program: %v", err)
	}
}

// TestServeRunsUntilTheGrantEnds: `txco sandbox serve` asks only for the
// list, serves, writes its pid, and when the grant ends removes the files
// and the pid file and exits 0.
func TestServeRunsUntilTheGrantEnds(t *testing.T) {
	fastPoll(t)
	dir := t.TempDir()
	newSandboxRig(t, append(grantEnviron, "HOME="+dir)...)
	fc := &fakeGrantChassis{token: "ghu_x", serve: []string{"dotenv:run.env"}}
	sandboxOpen = func(ctx context.Context, socket string, req grantwire.Request) (grantwire.Opened, error) {
		return fc.open(ctx, req)
	}
	pid := filepath.Join(dir, "run.pid")
	done := make(chan int, 1)
	var errb syncBuf
	go func() { done <- runSandbox([]string{"serve", "--dir", dir, "--pid", pid, "github"}, io.Discard, &errb) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(pid); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(os.Getpid()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid file; stderr %q", errb.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n, asks := fc.seen(); n != 0 || asks[0].Entry != grantwire.EntryList {
		t.Errorf("serve released before a read: %+v", asks)
	}
	if got := readWithin(t, filepath.Join(dir, "run.env")); !strings.HasPrefix(got, "GH_TOKEN=ghu_x\n") {
		t.Errorf("read %q", got)
	}
	fc.set("", true)
	_ = readWithin(t, filepath.Join(dir, "run.env"))
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not end with the grant")
	}
	for _, p := range []string{pid, filepath.Join(dir, "run.env")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind", p)
		}
	}
	if !strings.Contains(errb.String(), "the run's grant has ended") {
		t.Errorf("stderr %q", errb.String())
	}

	// A sandbox that serves no file, or one refused: serve does not start.
	fc = &fakeGrantChassis{}
	if code := runSandbox([]string{"serve", "--dir", dir, "github"}, io.Discard, io.Discard); code != sandboxExitUsage {
		t.Errorf("no files: exit %d", code)
	}
	fc.set("", true)
	if code := runSandbox([]string{"serve", "--dir", dir, "github"}, io.Discard, io.Discard); code != sandboxExitRefused {
		t.Errorf("refused: exit %d", code)
	}
}

// TestStartChildPassesTheExitCode: the real child runner answers the
// program's own exit code.
func TestStartChildPassesTheExitCode(t *testing.T) {
	if code := startChild("/bin/sh", []string{"sh", "-c", "exit 7"}, os.Environ(), io.Discard); code != 7 {
		t.Errorf("exit %d", code)
	}
	if code := startChild("/nonexistent/prog", []string{"prog"}, nil, io.Discard); code != sandboxExitCannotStart {
		t.Errorf("cannot start: %d", code)
	}
}

func TestDotenvLines(t *testing.T) {
	got := string(dotenvLines(map[string][]byte{"B": []byte("plain-1.2/x:y@z"), "A": []byte("two words"), "C": []byte("line\nnext"), "D": []byte("")}))
	want := "A='two words'\nB=plain-1.2/x:y@z\nC='line\nnext'\nD=\n"
	if got != want {
		t.Errorf("dotenv = %q, want %q", got, want)
	}
}
