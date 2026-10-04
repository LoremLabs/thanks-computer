package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// wrapper writes a prefix program that says which machine it was asked for
// (its first argument, on stderr) and then runs the rest of its argv — a
// stand-in for `sprite exec -s <name> --` that stays on this machine.
func wrapper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wrap.sh")
	body := "#!/bin/sh\nprintf 'MACHINE:%s\\n' \"$1\" >&2\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func newViaComputer(t *testing.T) (*Provider, workspace.Computer) {
	t.Helper()
	p, err := NewVia(filepath.Join(t.TempDir(), "ws"), wrapper(t)+" dev-{tenant}-{stack}-{name}")
	if err != nil {
		t.Fatal(err)
	}
	h, err := p.Create(context.Background(), workspace.Spec{Tenant: "acme", Stack: "loop", Name: "pony/intern"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Wake(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	return p, c
}

func TestViaRunsThroughThePrefix(t *testing.T) {
	_, c := newViaComputer(t)
	res := run(t, c, workspace.ExecRequest{Command: "echo hello; exit 3"})
	if got := strings.TrimSpace(string(res.Stdout)); got != "hello" {
		t.Errorf("stdout = %q, want hello", got)
	}
	if res.Exit != 3 {
		t.Errorf("exit = %d, want the command's own 3", res.Exit)
	}
	// The placeholders name the workspace; a slash in the name is a dash.
	if got := strings.TrimSpace(string(res.Stderr)); got != "MACHINE:dev-acme-loop-pony-intern" {
		t.Errorf("the prefix was asked for %q", got)
	}
}

func TestViaCarriesVariablesInTheCommand(t *testing.T) {
	_, c := newViaComputer(t)
	t.Setenv("VIA_CHASSIS_ONLY", "the chassis's own")
	res := run(t, c, workspace.ExecRequest{
		Command: `printf '%s|%s|%s' "$GREETING" "$QUOTED" "${VIA_CHASSIS_ONLY:-unset}"`,
		Env:     map[string]string{"GREETING": "hi there", "QUOTED": `it's "a" $HOME`},
	})
	// The request's variables are exported inside the command, exactly; the
	// prefix program runs with the chassis's environment (it is the
	// operator's program), and this stand-in passes that on.
	if got := string(res.Stdout); got != `hi there|it's "a" $HOME|the chassis's own` {
		t.Errorf("stdout = %q", got)
	}
}

func TestViaDoesNotUseTheWorkspaceAsHome(t *testing.T) {
	p, c := newViaComputer(t)
	res := run(t, c, workspace.ExecRequest{Command: `printf '%s' "$HOME"`})
	if strings.HasPrefix(string(res.Stdout), p.Root()) {
		t.Errorf("HOME = %q: the workspace directory is this machine's, not the command's", res.Stdout)
	}
}

func TestViaForwardsStdinAndArgs(t *testing.T) {
	_, c := newViaComputer(t)
	res := run(t, c, workspace.ExecRequest{Command: "cat", Stdin: []byte("the packet")})
	if string(res.Stdout) != "the packet" {
		t.Errorf("stdin came back as %q", res.Stdout)
	}
	res = run(t, c, workspace.ExecRequest{Args: []string{"printf", "%s-%s", "a b", "$c"}, Env: map[string]string{"X": "1"}})
	if string(res.Stdout) != "a b-$c" {
		t.Errorf("args came back as %q", res.Stdout)
	}
}

func TestViaCwdIsRelativeOnTheOtherMachine(t *testing.T) {
	p, c := newViaComputer(t)
	dir := filepath.Join(p.Root(), "acme", "loop", "pony", "intern")
	// The stand-in starts the command in the workspace directory (the prefix
	// program's own cwd), so a relative cd lands beneath it.
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, cwd := range []string{"work", filepath.Join(dir, "work")} {
		res := run(t, c, workspace.ExecRequest{Command: "basename \"$PWD\"", Cwd: cwd})
		if got := strings.TrimSpace(string(res.Stdout)); got != "work" {
			t.Errorf("cwd %q: ran in %q", cwd, got)
		}
	}
	if _, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "true", Cwd: "../.."}, workspace.Limits{}); err == nil {
		t.Error("a cwd that climbs out of the workspace was accepted")
	}
	res := run(t, c, workspace.ExecRequest{Command: "echo no", Cwd: "nonesuch"})
	if res.Exit != 127 {
		t.Errorf("a cwd that is not there: exit %d, want 127", res.Exit)
	}
}

func TestViaRefusesWhatWouldRunHere(t *testing.T) {
	p, c := newViaComputer(t)
	if p.ReachesGrants() {
		t.Error("ReachesGrants with a prefix: the command is on another machine")
	}
	if got := strings.Join(p.Capabilities(), ","); got != "exec,grant" {
		t.Errorf("capabilities = %s", got)
	}
	var we *workspace.Error
	if _, err := c.(workspace.Starter).Start(context.Background(), workspace.ExecRequest{Command: "sh"}, workspace.Limits{}); !errors.As(err, &we) || we.Code != "unsupported" {
		t.Errorf("a session through a prefix: %v", err)
	}
	if _, err := c.(workspace.Dialer).DialService(context.Background(), workspace.Service{Name: "web", Port: 80}); !errors.As(err, &we) || we.Code != "unsupported" {
		t.Errorf("a dial through a prefix: %v", err)
	}
	if _, err := c.(workspace.TreeHolder).PlaceTree(context.Background(), workspace.Tree{Digest: "x"}); !errors.As(err, &we) || we.Code != workspace.CodeStackDirUnavailable {
		t.Errorf("a stack tree through a prefix: %v", err)
	}
	if _, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "true", Env: map[string]string{"BAD-NAME": "x"}}, workspace.Limits{}); !errors.As(err, &we) || we.Code != "bad_request" {
		t.Errorf("a variable that cannot be exported: %v", err)
	}
}

func TestViaTimeoutKillsThePrefix(t *testing.T) {
	_, c := newViaComputer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Exec(ctx, workspace.ExecRequest{Command: "sleep 30"}, workspace.Limits{})
	if !errors.Is(err, workspace.ErrTimeout) {
		t.Errorf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the prefix outlived its deadline by %s", time.Since(start))
	}
}

func TestNoPrefixIsTheProviderAsItWas(t *testing.T) {
	p, c, _ := newComputer(t)
	if !p.ReachesGrants() || !strings.Contains(strings.Join(p.Capabilities(), ","), "session") {
		t.Error("without a prefix the provider lost a capability")
	}
	res := run(t, c, workspace.ExecRequest{Command: `printf '%s' "$HOME"`})
	if !strings.HasPrefix(string(res.Stdout), p.Root()) {
		t.Errorf("HOME = %q, want the workspace directory", res.Stdout)
	}
}
