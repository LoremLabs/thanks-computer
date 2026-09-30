package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
)

// sandboxRig replaces the launcher's view of the world: what it may ask the
// chassis, where programs are, and what starting one does.
type sandboxRig struct {
	asks    []grantwire.Request
	sockets []string
	answer  func(grantwire.Request) (map[string][]byte, error)

	started bool
	path    string
	argv    []string
	env     []string
	execErr error
}

func newSandboxRig(t *testing.T, environ ...string) *sandboxRig {
	t.Helper()
	r := &sandboxRig{}
	oldOpen, oldExec, oldLook, oldEnviron, oldGetenv := sandboxOpen, sandboxExec, sandboxLook, sandboxEnviron, sandboxGetenv
	t.Cleanup(func() {
		sandboxOpen, sandboxExec, sandboxLook, sandboxEnviron, sandboxGetenv = oldOpen, oldExec, oldLook, oldEnviron, oldGetenv
	})
	sandboxOpen = func(_ context.Context, socket string, req grantwire.Request) (map[string][]byte, error) {
		r.asks = append(r.asks, req)
		r.sockets = append(r.sockets, socket)
		if r.answer == nil {
			// Every sandbox sets one variable named for it, in upper case.
			return map[string][]byte{strings.ToUpper(req.Sandbox) + "_TOKEN": []byte("value-of-" + req.Sandbox)}, nil
		}
		return r.answer(req)
	}
	sandboxExec = func(path string, argv, env []string) error {
		r.started, r.path, r.argv, r.env = true, path, argv, env
		return r.execErr
	}
	sandboxLook = func(name string) (string, error) {
		if name == "missing" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	sandboxEnviron = func() []string { return environ }
	sandboxGetenv = func(k string) string {
		for _, kv := range environ {
			if name, v, _ := strings.Cut(kv, "="); name == k {
				return v
			}
		}
		return ""
	}
	return r
}

var grantEnviron = []string{
	"PATH=/usr/bin", "HOME=/ws", "GITHUB_TOKEN=stale",
	"TXCO_RUN=task-1", "TXCO_RUN_GRANT=rg1.claims.mac", "TXCO_GRANT_SOCK=/tmp/txco-1/g.sock",
	"TXCO_BIN=/usr/local/bin/txco",
}

func runSandboxIn(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = runSandbox(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestSandboxStartsTheProgramInside(t *testing.T) {
	r := newSandboxRig(t, grantEnviron...)
	code, _, stderr := runSandboxIn("github", "postgres", "github", "--", "git", "push", "--force")
	if code != 0 || stderr != "" || !r.started {
		t.Fatalf("exit %d, started=%v, stderr %q", code, r.started, stderr)
	}
	if r.path != "/usr/bin/git" || strings.Join(r.argv, " ") != "git push --force" {
		t.Errorf("started %s %v", r.path, r.argv)
	}
	// Each sandbox was one request, made with the grant's token, once.
	if len(r.asks) != 2 || r.asks[0].Sandbox != "github" || r.asks[1].Sandbox != "postgres" {
		t.Fatalf("asks = %+v", r.asks)
	}
	for i, a := range r.asks {
		if a.Token != "rg1.claims.mac" || r.sockets[i] != "/tmp/txco-1/g.sock" {
			t.Errorf("ask %d = %+v on %s", i, a, r.sockets[i])
		}
	}

	env := envOf(r.env)
	for k, want := range map[string]string{
		"GITHUB_TOKEN":   "value-of-github", // replaced, not added beside the stale one
		"POSTGRES_TOKEN": "value-of-postgres",
		"PATH":           "/usr/bin", "HOME": "/ws",
	} {
		if env[k] != want {
			t.Errorf("the program's %s = %q, want %q", k, env[k], want)
		}
	}
	// The program can open nothing more: none of the grant's variables.
	for _, k := range grantwire.Env {
		if v, has := env[k]; has {
			t.Errorf("the program was given %s=%q", k, v)
		}
	}
	n := 0
	for _, kv := range r.env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GITHUB_TOKEN is set %d times", n)
	}
}

func TestSandboxRefusedStartsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		answer func(grantwire.Request) (map[string][]byte, error)
		code   int
		says   string
	}{
		"refused": {func(grantwire.Request) (map[string][]byte, error) { return nil, grantwire.ErrRefused }, sandboxExitRefused, "first: refused"},
		"unavailable": {func(grantwire.Request) (map[string][]byte, error) {
			return nil, fmt.Errorf("%w: dial: no such file", grantwire.ErrUnavailable)
		}, sandboxExitUnavailable, "unavailable"},
		"the second sandbox is refused": {func(q grantwire.Request) (map[string][]byte, error) {
			if q.Sandbox == "postgres" {
				return nil, grantwire.ErrRefused
			}
			return map[string][]byte{"V": []byte("v")}, nil
		}, sandboxExitRefused, "postgres: refused"},
		"two sandboxes set one variable": {func(q grantwire.Request) (map[string][]byte, error) {
			return map[string][]byte{"TOKEN": []byte("v")}, nil
		}, sandboxExitUsage, "sandboxes first and postgres both set TOKEN"},
	} {
		r := newSandboxRig(t, grantEnviron...)
		r.answer = tc.answer
		code, stdout, stderr := runSandboxIn("first", "postgres", "--", "git", "push")
		if code != tc.code || r.started || !strings.Contains(stderr, tc.says) || stdout != "" {
			t.Errorf("%s: exit %d, started=%v, stderr %q", name, code, r.started, stderr)
		}
		// A refusal says what was refused and never why.
		if name != "unavailable" && strings.Count(stderr, "\n") != 1 {
			t.Errorf("%s: stderr says more than the refusal: %q", name, stderr)
		}
	}
}

// A value with a NUL in it would end the variable early, or stop the
// program from starting with a message about an argument. Say what is wrong.
func TestSandboxAValueNoVariableCanHold(t *testing.T) {
	r := newSandboxRig(t, grantEnviron...)
	r.answer = func(q grantwire.Request) (map[string][]byte, error) {
		return map[string][]byte{"KEY": []byte("abc\x00def")}, nil
	}
	code, _, stderr := runSandboxIn("signing", "--", "git")
	if code != sandboxExitData || r.started || !strings.Contains(stderr, "signing: KEY holds a NUL byte") {
		t.Errorf("exit %d, started=%v, stderr %q", code, r.started, stderr)
	}
	if strings.Contains(stderr, "abc") || strings.Contains(stderr, "def") {
		t.Errorf("the message prints the secret: %q", stderr)
	}
	// Any other byte is the program's to make sense of.
	r = newSandboxRig(t, grantEnviron...)
	r.answer = func(q grantwire.Request) (map[string][]byte, error) {
		return map[string][]byte{"KEY": []byte("line one\nline two\xff=")}, nil
	}
	if code, _, stderr := runSandboxIn("signing", "--", "git"); code != 0 || envOf(r.env)["KEY"] != "line one\nline two\xff=" {
		t.Errorf("exit %d, KEY %q, stderr %q", code, envOf(r.env)["KEY"], stderr)
	}
}

func TestSandboxNeedsAGrantAndAChassis(t *testing.T) {
	for name, tc := range map[string]struct {
		environ []string
		says    string
	}{
		"no grant":  {[]string{"PATH=/usr/bin", "TXCO_GRANT_SOCK=/tmp/g.sock"}, "no run grant in this environment"},
		"no socket": {[]string{"PATH=/usr/bin", "TXCO_RUN_GRANT=rg1.a.b"}, "not listening for the launcher"},
	} {
		r := newSandboxRig(t, tc.environ...)
		code, _, stderr := runSandboxIn("github", "--", "git")
		if code != sandboxExitUnavailable || r.started || len(r.asks) != 0 || !strings.Contains(stderr, tc.says) {
			t.Errorf("%s: exit %d, started=%v, asks=%d, stderr %q", name, code, r.started, len(r.asks), stderr)
		}
	}
}

func TestSandboxTheProgram(t *testing.T) {
	// A program that is not there is found out before anything is asked for.
	r := newSandboxRig(t, grantEnviron...)
	code, _, stderr := runSandboxIn("github", "--", "missing", "arg")
	if code != sandboxExitNotFound || len(r.asks) != 0 || r.started || !strings.Contains(stderr, "missing: not found") {
		t.Errorf("not found: exit %d, asks=%d, stderr %q", code, len(r.asks), stderr)
	}
	r = newSandboxRig(t, grantEnviron...)
	r.execErr = errors.New("exec format error")
	code, _, stderr = runSandboxIn("github", "--", "git")
	if code != sandboxExitCannotStart || !strings.Contains(stderr, "exec format error") {
		t.Errorf("cannot start: exit %d, stderr %q", code, stderr)
	}
	if strings.Contains(stderr, "value-of-github") {
		t.Errorf("the failure prints the secret: %q", stderr)
	}
	// The program's own flags, and its own --, are the program's.
	r = newSandboxRig(t, grantEnviron...)
	if code, _, stderr := runSandboxIn("github", "--", "git", "-e", "--env", "--", "push"); code != 0 || strings.Join(r.argv, " ") != "git -e --env -- push" || len(r.asks) != 1 {
		t.Errorf("exit %d, argv %v, asks %d, stderr %q", code, r.argv, len(r.asks), stderr)
	}
}

func TestSandboxCommandLine(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		says string
	}{
		"nothing":               {nil, "no sandbox named"},
		"no program":            {[]string{"github"}, "no program to start"},
		"no -- before it":       {[]string{"github", "git", "push"}, "put -- between"},
		"no program after --":   {[]string{"github", "--"}, "no program to start"},
		"a program and no name": {[]string{"--", "git"}, "no sandbox named"},
		"not a name":            {[]string{"GitHub", "--", "git"}, "is not a sandbox name"},
		"a path":                {[]string{"./github", "--", "git"}, "is not a sandbox name"},
		"an unknown flag":       {[]string{"--env", "A=secret:X", "--", "git"}, "unknown flag --env"},
	} {
		r := newSandboxRig(t, grantEnviron...)
		code, _, stderr := runSandboxIn(tc.args...)
		if code != sandboxExitUsage || r.started || len(r.asks) != 0 || !strings.Contains(stderr, tc.says) {
			t.Errorf("%s: exit %d, started=%v, stderr %q", name, code, r.started, stderr)
		}
	}
	r := newSandboxRig(t, grantEnviron...)
	if code, stdout, _ := runSandboxIn("--help"); code != 0 || !strings.Contains(stdout, "Usage: txco sandbox") || r.started {
		t.Errorf("--help: exit %d, stdout %q", code, stdout)
	}
	// It is a built-in, so no plugin and no chassis is asked about it.
	var out, errb bytes.Buffer
	if code, ok := Dispatch([]string{"txco", "sandbox", "--help"}, &out, &errb); !ok || code != 0 || !strings.Contains(out.String(), "Usage: txco sandbox") {
		t.Errorf("dispatch: exit %d ok=%v out %q", code, ok, out.String())
	}
}
