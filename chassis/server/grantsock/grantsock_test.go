package grantsock

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/server/grantgw"
)

// fakeGateway answers by the sandbox asked for.
type fakeGateway struct {
	mu     sync.Mutex
	asks   []grantgw.Open
	socket string
}

func (g *fakeGateway) OpenSandbox(_ context.Context, o grantgw.Open) grantgw.Answer {
	g.mu.Lock()
	g.asks = append(g.asks, o)
	g.mu.Unlock()
	switch o.Sandbox {
	case "refused":
		return grantgw.Answer{Reason: "pull"}
	case "down":
		return grantgw.Answer{Unavailable: true, Reason: "secret store"}
	case "binary":
		return grantgw.Answer{Allowed: true, Env: map[string][]byte{"B": {0, 1, 2, '\n', 0xff, '"', '\\'}}}
	case "big":
		return grantgw.Answer{Allowed: true, Env: map[string][]byte{"K": []byte(strings.Repeat("k", 1<<20))}}
	case "two":
		return grantgw.Answer{Allowed: true, Env: map[string][]byte{"A": []byte("a"), "B": []byte("b")}}
	}
	return grantgw.Answer{Allowed: true, Env: map[string][]byte{"V": []byte("value-of-" + o.Sandbox)}}
}

func (g *fakeGateway) ServeSocket(path string) {
	g.mu.Lock()
	g.socket = path
	g.mu.Unlock()
}

func (g *fakeGateway) served() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.socket
}

// shortTemp is a temp dir whose path is short enough to hold a socket: the
// test's own can run past 100 bytes on macOS.
func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "txgs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func start(t *testing.T, gw *fakeGateway) *Controller {
	t.Helper()
	c, err := New(context.Background(), nil, gw, filepath.Join(shortTemp(t), "d", "g.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	c.Start()
	t.Cleanup(c.Stop)
	return c
}

func TestTheLauncherAsksOverTheSocket(t *testing.T) {
	gw := &fakeGateway{}
	c := start(t, gw)
	ctx := context.Background()
	if gw.served() != c.Path() {
		t.Errorf("the gateway was told %q, the socket is %q", gw.served(), c.Path())
	}

	env, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "rg1.a.b", Sandbox: "github"})
	if err != nil || string(env["V"]) != "value-of-github" || len(env) != 1 {
		t.Fatalf("open: %q err=%v", env, err)
	}
	gw.mu.Lock()
	got := gw.asks[0]
	gw.mu.Unlock()
	if got.Token != "rg1.a.b" || got.GrantID != "" || got.TenantID != "" || got.Sandbox != "github" || got.Via != grantgw.ViaLauncher {
		t.Errorf("the gateway was asked %+v", got)
	}

	// A value is bytes: it survives whatever it holds, and its size.
	if env, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "binary"}); err != nil ||
		string(env["B"]) != "\x00\x01\x02\n\xff\"\\" {
		t.Errorf("binary: %q err=%v", env, err)
	}
	if env, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "big"}); err != nil || len(env["K"]) != 1<<20 {
		t.Errorf("a megabyte: %d bytes err=%v", len(env["K"]), err)
	}
	if env, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "two"}); err != nil || string(env["A"]) != "a" || string(env["B"]) != "b" {
		t.Errorf("two variables: %q err=%v", env, err)
	}

	if _, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "refused"}); !errors.Is(err, grantwire.ErrRefused) {
		t.Errorf("refused: %v", err)
	} else if strings.Contains(err.Error(), "pull") {
		t.Errorf("the refusal says why: %v", err)
	}
	if _, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "down"}); !errors.Is(err, grantwire.ErrUnavailable) {
		t.Errorf("unavailable: %v", err)
	} else if strings.Contains(err.Error(), "secret store") {
		t.Errorf("the failure says what failed inside the chassis: %v", err)
	}

	// Many at once.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if env, err := grantwire.Open(ctx, c.Path(), grantwire.Request{Token: "t", Sandbox: "github"}); err != nil || string(env["V"]) != "value-of-github" {
				t.Errorf("concurrent open: %q err=%v", env, err)
			}
		}()
	}
	wg.Wait()
}

// raw sends bytes and returns the one line that comes back.
func raw(t *testing.T, path, send string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(send)); err != nil {
		return "write: " + err.Error()
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}

func TestWhatIsNotARequestIsRefusedAsOne(t *testing.T) {
	gw := &fakeGateway{}
	c := start(t, gw)
	for name, send := range map[string]string{
		"not JSON":         "hello\n",
		"an empty line":    "\n",
		"no token":         `{"v":2,"sandbox":"github"}` + "\n",
		"no sandbox":       `{"v":2,"token":"t"}` + "\n",
		"a bad sandbox":    `{"v":2,"token":"t","sandbox":"../GitHub"}` + "\n",
		"a secret by name": `{"v":1,"token":"t","kind":"secret","name":"DB_DSN"}` + "\n",
		"v2 with a name":   `{"v":2,"token":"t","kind":"secret","name":"DB_DSN"}` + "\n",
		"another version":  `{"v":3,"token":"t","sandbox":"github"}` + "\n",
		"no version":       `{"token":"t","sandbox":"github"}` + "\n",
		"an array":         `[1]` + "\n",
		"a line too long":  `{"v":2,"token":"` + strings.Repeat("t", grantwire.MaxRequest) + `"}` + "\n",
		"an HTTP request":  "GET / HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		if got := raw(t, c.Path(), send); got != `{"ok":false,"error":"bad_request"}` {
			t.Errorf("%s: %q", name, got)
		}
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	if len(gw.asks) != 0 {
		t.Errorf("the gateway was asked %d times for things that are not requests", len(gw.asks))
	}
}

func TestTheSocketIsThisUsersAlone(t *testing.T) {
	gw := &fakeGateway{}
	dir := filepath.Join(shortTemp(t), "open")
	// A directory that was left open to everyone is closed.
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), nil, gw, filepath.Join(dir, "g.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	c.Start()
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the socket's directory is %v, want 0700", fi.Mode().Perm())
	}
	if fi, err := os.Stat(c.Path()); err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("the socket is %v, want a socket at 0600", fi.Mode())
	}

	// Another chassis must not take a socket that is in use…
	second, _ := New(context.Background(), nil, &fakeGateway{}, c.Path())
	if err := second.Listen(); err == nil || !strings.Contains(err.Error(), "another process is listening") {
		t.Errorf("a second listener: %v", err)
	}
	// …and the first still answers.
	if env, err := grantwire.Open(context.Background(), c.Path(), grantwire.Request{Token: "t", Sandbox: "x"}); err != nil || string(env["V"]) != "value-of-x" {
		t.Errorf("after a refused second listener: %q err=%v", env, err)
	}

	// Stopped, it is gone, and the gateway stops handing it out.
	c.Stop()
	if _, err := os.Stat(c.Path()); !os.IsNotExist(err) {
		t.Errorf("the socket outlived its chassis: %v", err)
	}
	if gw.served() != "" {
		t.Errorf("the gateway still hands out %q", gw.served())
	}
	if _, err := grantwire.Open(context.Background(), c.Path(), grantwire.Request{Token: "t", Sandbox: "x"}); !errors.Is(err, grantwire.ErrUnavailable) {
		t.Errorf("asking a stopped chassis: %v", err)
	}

	// A socket left by a chassis that was killed is taken over.
	ln, err := net.Listen("unix", c.Path())
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	third, _ := New(context.Background(), nil, gw, c.Path())
	if err := third.Listen(); err != nil {
		t.Fatalf("over a stale socket: %v", err)
	}
	third.Start()
	defer third.Stop()
	if env, err := grantwire.Open(context.Background(), c.Path(), grantwire.Request{Token: "t", Sandbox: "x"}); err != nil || string(env["V"]) != "value-of-x" {
		t.Errorf("over a stale socket: %q err=%v", env, err)
	}
}

func TestWhatCannotBeASocket(t *testing.T) {
	gw := &fakeGateway{}
	long := filepath.Join("/tmp", strings.Repeat("d", 90), "g.sock")
	if _, err := New(context.Background(), nil, gw, long); err == nil || !strings.Contains(err.Error(), "at most 100") {
		t.Errorf("a path too long: %v", err)
	}
	if _, err := New(context.Background(), nil, nil, "/tmp/g.sock"); err == nil || !strings.Contains(err.Error(), "no grant gateway") {
		t.Errorf("no gateway: %v", err)
	}
	// A file that is not a socket is not removed to make room for one.
	path := filepath.Join(shortTemp(t), "g.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := New(context.Background(), nil, gw, path)
	if err := c.Listen(); err == nil || !strings.Contains(err.Error(), "is not a socket") {
		t.Errorf("over a file: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep me" {
		t.Errorf("the file was touched: %q", b)
	}

	// Two chassis on one machine do not share a socket, and a restart
	// finds the same one.
	a, b := DefaultPath("/srv/a/data"), DefaultPath("/srv/b/data")
	if a == b || a != DefaultPath("/srv/a/data") || filepath.Base(a) != "g.sock" || len(a) > grantwire.MaxSocketPath {
		t.Errorf("default paths: %q %q", a, b)
	}
}
