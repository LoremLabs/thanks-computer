package local

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func newComputer(t *testing.T) (*Provider, workspace.Computer, workspace.Handle) {
	t.Helper()
	p, err := New(filepath.Join(t.TempDir(), "ws"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Create(context.Background(), workspace.Spec{Tenant: "acme", Stack: "agents", Name: "tools"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Wake(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	return p, c, h
}

func run(t *testing.T, c workspace.Computer, req workspace.ExecRequest) workspace.ExecResult {
	t.Helper()
	res, err := c.Exec(context.Background(), req, workspace.Limits{})
	if err != nil {
		t.Fatalf("Exec %+v: %v", req, err)
	}
	return res
}

func TestExitCodesAreData(t *testing.T) {
	_, c, _ := newComputer(t)
	if res := run(t, c, workspace.ExecRequest{Command: "exit 3"}); res.Exit != 3 {
		t.Errorf("exit = %d, want 3", res.Exit)
	}
	if res := run(t, c, workspace.ExecRequest{Command: "true"}); res.Exit != 0 {
		t.Errorf("exit = %d, want 0", res.Exit)
	}
}

func TestStdoutStderrSplitAndStdin(t *testing.T) {
	_, c, _ := newComputer(t)
	res := run(t, c, workspace.ExecRequest{Command: "echo out; echo err >&2"})
	if string(res.Stdout) != "out\n" || string(res.Stderr) != "err\n" {
		t.Errorf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	res = run(t, c, workspace.ExecRequest{Command: "cat", Stdin: []byte("fed in")})
	if string(res.Stdout) != "fed in" {
		t.Errorf("stdin not delivered: %q", res.Stdout)
	}
}

func TestArgvModeNoShell(t *testing.T) {
	_, c, _ := newComputer(t)
	// No shell: the $HOME is passed literally, not expanded.
	res := run(t, c, workspace.ExecRequest{Args: []string{"/bin/echo", "$HOME"}})
	if string(res.Stdout) != "$HOME\n" {
		t.Errorf("argv mode expanded through a shell: %q", res.Stdout)
	}
	if _, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "x", Args: []string{"y"}}, workspace.Limits{}); err == nil {
		t.Error("command+args accepted")
	}
	if _, err := c.Exec(context.Background(), workspace.ExecRequest{}, workspace.Limits{}); err == nil {
		t.Error("empty request accepted")
	}
	_, err := c.Exec(context.Background(), workspace.ExecRequest{Args: []string{"/no/such/binary"}}, workspace.Limits{})
	var we *workspace.Error
	if !errors.As(err, &we) || we.Code != "bad_request" {
		t.Errorf("missing binary: err = %v, want bad_request", err)
	}
}

func TestOutputTruncation(t *testing.T) {
	_, c, _ := newComputer(t)
	res, err := c.Exec(context.Background(),
		workspace.ExecRequest{Command: "head -c 5000 /dev/zero | tr '\\0' x; head -c 3 /dev/zero | tr '\\0' y >&2"},
		workspace.Limits{MaxOutputBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 100 || !res.StdoutTruncated {
		t.Errorf("stdout len=%d truncated=%v, want 100/true", len(res.Stdout), res.StdoutTruncated)
	}
	if len(res.Stderr) != 3 || res.StderrTruncated {
		t.Errorf("stderr len=%d truncated=%v, want 3/false", len(res.Stderr), res.StderrTruncated)
	}
	if res.Exit != 0 {
		t.Errorf("exit = %d after truncation, want 0 (the child must not see EPIPE)", res.Exit)
	}
}

func TestCwdGuard(t *testing.T) {
	_, c, h := newComputer(t)
	if err := os.MkdirAll(filepath.Join(h.Ref, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	res := run(t, c, workspace.ExecRequest{Command: "pwd", Cwd: "sub"})
	if strings.TrimSpace(string(res.Stdout)) != filepath.Join(h.Ref, "sub") {
		t.Errorf("cwd = %q, want %q", res.Stdout, filepath.Join(h.Ref, "sub"))
	}
	for _, bad := range []string{"..", "../..", "sub/../..", "/etc", "missing"} {
		_, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "pwd", Cwd: bad}, workspace.Limits{})
		var we *workspace.Error
		if !errors.As(err, &we) || we.Code != "bad_request" {
			t.Errorf("cwd %q: err = %v, want bad_request", bad, err)
		}
	}
	// A symlink that points outside is caught after resolution.
	if err := os.Symlink(os.TempDir(), filepath.Join(h.Ref, "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "pwd", Cwd: "out"}, workspace.Limits{}); err == nil {
		t.Error("symlink escape accepted")
	}
}

func TestEnvIsolation(t *testing.T) {
	t.Setenv("WS_CANARY", "leaked")
	_, c, h := newComputer(t)
	res := run(t, c, workspace.ExecRequest{Command: "env", Env: map[string]string{"GREETING": "hi"}})
	out := string(res.Stdout)
	if strings.Contains(out, "WS_CANARY") {
		t.Errorf("chassis environment leaked into the workspace:\n%s", out)
	}
	for _, want := range []string{"HOME=" + h.Ref + "\n", "TMPDIR=" + filepath.Join(h.Ref, "tmp") + "\n", "PATH=" + basePath + "\n", "GREETING=hi\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("env missing %q:\n%s", want, out)
		}
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	_, c, _ := newComputer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := c.Exec(ctx, workspace.ExecRequest{Command: "sleep 10 & sleep 10"}, workspace.Limits{})
	if !errors.Is(err, workspace.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if res.Exit != -1 {
		t.Errorf("exit = %d, want -1", res.Exit)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("timeout took %v; the process group was not killed", took)
	}
}

func TestPersistenceAcrossExecs(t *testing.T) {
	p, c, h := newComputer(t)
	run(t, c, workspace.ExecRequest{Command: "echo kept > note.txt"})
	// A second wake of the same handle sees the file.
	c2, err := p.Wake(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, c2, workspace.ExecRequest{Command: "cat note.txt"})
	if string(res.Stdout) != "kept\n" {
		t.Errorf("file did not persist: %q", res.Stdout)
	}
	st, err := p.Status(context.Background(), h)
	if err != nil || st.State != "running" {
		t.Errorf("status = %+v err=%v, want running", st, err)
	}
	if err := p.Destroy(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Ref); !os.IsNotExist(err) {
		t.Error("destroy left the directory")
	}
	st, _ = p.Status(context.Background(), h)
	if st.State != "destroyed" {
		t.Errorf("status after destroy = %q", st.State)
	}
	if _, err := p.Wake(context.Background(), h); err == nil {
		t.Error("wake of a destroyed workspace succeeded")
	}
}

func TestCreateIsIdempotentAndGuarded(t *testing.T) {
	p, _, h := newComputer(t)
	h2, err := p.Create(context.Background(), workspace.Spec{Tenant: "acme", Stack: "agents", Name: "tools"})
	if err != nil || h2.Ref != h.Ref {
		t.Errorf("second create: %+v %v", h2, err)
	}
	for _, spec := range []workspace.Spec{
		{Tenant: "", Stack: "s", Name: "n"},
		{Tenant: "..", Stack: "s", Name: "n"},
		{Tenant: "a/b", Stack: "s", Name: "n"},
		{Tenant: "t", Stack: "s", Name: "../../etc"},
		{Tenant: "t", Stack: "s", Name: "Bad"},
	} {
		if _, err := p.Create(context.Background(), spec); err == nil {
			t.Errorf("Create(%+v) accepted", spec)
		}
	}
	// Destroy never reaches outside the root.
	if err := p.Destroy(context.Background(), workspace.Handle{Ref: filepath.Dir(p.Root())}); err == nil {
		t.Error("destroy outside root accepted")
	}
	if err := p.Destroy(context.Background(), workspace.Handle{Ref: p.Root()}); err == nil {
		t.Error("destroy of the root accepted")
	}
}

func TestRegistered(t *testing.T) {
	p, err := workspace.Open("local", workspace.Config{LocalRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "local" {
		t.Errorf("name = %q", p.Name())
	}
}

// TestDialServiceRoundTripsAndReportsUnavailable: a listener on this
// host's loopback is reachable by port through DialService; a port with
// nothing behind it is "unavailable", not a provider fault.
func TestDialServiceRoundTripsAndReportsUnavailable(t *testing.T) {
	_, c, _ := newComputer(t)
	d, ok := c.(workspace.Dialer)
	if !ok {
		t.Fatal("local computer does not implement workspace.Dialer")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn) // echo
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	svc := workspace.Service{Name: "echo", Port: port}
	conn, err := d.DialService(context.Background(), svc)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q err=%v", buf, err)
	}

	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	_, err = d.DialService(context.Background(), workspace.Service{Name: "gone", Port: dead})
	var we *workspace.Error
	if !errors.As(err, &we) || we.Code != "unavailable" {
		t.Fatalf("dead port err = %v, want unavailable", err)
	}
	if _, err := d.DialService(context.Background(), workspace.Service{Name: "x"}); err == nil {
		t.Fatal("a zero port must be refused")
	}
}
