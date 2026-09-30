package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// `txco sandbox` starts a program inside named sandboxes: what each one
// sets is asked for with the run grant this command was itself started
// with, and put in the program's environment.
//
//	txco sandbox github -- gh pr list
//	txco sandbox github postgres -- ./migrate
//
// It runs inside a workspace, started by a rule's `workspace://…/exec WITH
// grant`. A sandbox is a declaration of the stack (SANDBOXES/<name>.yaml):
// the program names the sandbox and never a secret. Each secret the sandbox
// names is one request to the chassis, decided like any other made with the
// grant — by the tenant's `_grant` stack, starting from the chassis's
// proposal — and leaves the same trace. A sandbox opens whole or not at all.

// The launcher's own exit codes, from sysexits.h and the shell. Once the
// program is running, every code is the program's.
const (
	sandboxExitUsage       = 2   // the command line
	sandboxExitData        = 65  // EX_DATAERR: a value that no variable can hold
	sandboxExitUnavailable = 69  // EX_UNAVAILABLE: the chassis could not be asked, or could not decide
	sandboxExitRefused     = 77  // EX_NOPERM: the chassis decided, and the answer is no
	sandboxExitCannotStart = 126 // the program was found and could not be started
	sandboxExitNotFound    = 127 // the program was not found
)

const sandboxUsage = `Usage: txco sandbox NAME [NAME…] -- PROGRAM [ARGS…]

Start PROGRAM inside the named sandboxes. Each NAME is a sandbox the stack
declares (SANDBOXES/NAME.yaml) and the run grant this command was started
with may open; what it sets is asked for and put in PROGRAM's environment.
A sandbox opens whole or not at all.

PROGRAM replaces this command: its exit code and its signals are its own. It
is given what the sandboxes set and none of the grant's own variables, so it
cannot open more.

Exit codes, before PROGRAM starts:
   2  the command line
  65  a value holds a byte no variable can (a NUL)
  69  the chassis could not be asked, or could not decide
  77  a sandbox was refused
 126  PROGRAM could not be started
 127  PROGRAM was not found

Runs inside a workspace, from a rule's "workspace://<name>/exec" WITH grant.
`

// parseSandbox reads the command line: the sandboxes, then `--`, then the
// program. The `--` is required: a program's name ("git", "make") is as
// good a sandbox name as any, so nothing else could tell them apart. It
// fails on anything it does not understand — a flag that was meant for the
// launcher and reached the program instead would be a surprise.
func parseSandbox(args []string) (names, argv []string, help bool, err error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			argv = args[i+1:]
			if len(argv) == 0 {
				return nil, nil, false, errors.New("no program to start after --")
			}
			if len(names) == 0 {
				return nil, nil, false, errors.New("no sandbox named before --")
			}
			return names, argv, false, nil
		case a == "-h" || a == "--help" || a == "help":
			return nil, nil, true, nil
		case strings.HasPrefix(a, "-"):
			return nil, nil, false, fmt.Errorf("unknown flag %s (put -- before the program's own flags)", a)
		case !sandbox.ValidName(a):
			return nil, nil, false, fmt.Errorf("%q is not a sandbox name (a name matching [a-z][a-z0-9_-]*): the program goes after --", a)
		case !seen[a]:
			seen[a] = true
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return nil, nil, false, errors.New("no sandbox named")
	}
	return nil, nil, false, errors.New("no program to start: put -- between the sandboxes and the program")
}

// sandboxEnv is the program's environment: this command's, less every
// variable of the grant, plus what the sandboxes set. A variable a sandbox
// sets replaces one of the same name that was already there.
func sandboxEnv(environ []string, got map[string]string) []string {
	drop := map[string]bool{}
	for _, k := range grantwire.Env {
		drop[k] = true
	}
	out := make([]string, 0, len(environ)+len(got))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if _, replaced := got[k]; drop[k] || replaced {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range sortedPairs(got) {
		out = append(out, kv)
	}
	return out
}

func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	// Small, and the order a program sees its environment in should not
	// change from one start to the next.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Seams for tests.
var (
	sandboxOpen    = grantwire.Open
	sandboxExec    = syscall.Exec
	sandboxLook    = exec.LookPath
	sandboxEnviron = os.Environ
	sandboxGetenv  = os.Getenv
)

func runSandbox(args []string, stdout, stderr io.Writer) int {
	names, argv, help, err := parseSandbox(args)
	if help {
		fmt.Fprint(stdout, sandboxUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %v\n\n%s", err, sandboxUsage)
		return sandboxExitUsage
	}
	token, socket := sandboxGetenv(grantwire.EnvToken), sandboxGetenv(grantwire.EnvSocket)
	switch {
	case token == "":
		fmt.Fprintf(stderr, "txco sandbox: no run grant in this environment (%s): start this command from a rule's exec WITH grant\n", grantwire.EnvToken)
		return sandboxExitUnavailable
	case socket == "":
		fmt.Fprintf(stderr, "txco sandbox: the chassis is not listening for the launcher (%s): add `grant` to its --personalities\n", grantwire.EnvSocket)
		return sandboxExitUnavailable
	}
	// Find the program before asking for anything: a sandbox opened for a
	// program that cannot start was opened for nothing.
	path, err := sandboxLook(argv[0])
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %s: not found\n", argv[0])
		return sandboxExitNotFound
	}

	got := map[string]string{}
	setBy := map[string]string{}
	for _, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		env, err := sandboxOpen(ctx, socket, grantwire.Request{Token: token, Sandbox: name})
		cancel()
		switch {
		case errors.Is(err, grantwire.ErrRefused):
			// Not why: the reason is in the trace, for whoever may read it.
			fmt.Fprintf(stderr, "txco sandbox: %s: refused\n", name)
			return sandboxExitRefused
		case err != nil:
			fmt.Fprintf(stderr, "txco sandbox: %s: %v\n", name, err)
			return sandboxExitUnavailable
		}
		code := 0
		for _, v := range sortedVars(env) {
			value := env[v]
			switch {
			case setBy[v] != "":
				fmt.Fprintf(stderr, "txco sandbox: sandboxes %s and %s both set %s\n", setBy[v], name, v)
				code = sandboxExitUsage
			case bytes.IndexByte(value, 0) >= 0:
				// An environment is a list of C strings: a NUL ends one. A
				// secret of random bytes holds one about one time in eight.
				fmt.Fprintf(stderr, "txco sandbox: %s: %s holds a NUL byte, which no environment variable can\n", name, v)
				code = sandboxExitData
			default:
				got[v] = string(value)
				setBy[v] = name
			}
			for i := range value {
				value[i] = 0
			}
		}
		if code != 0 {
			return code
		}
	}
	if err := sandboxExec(path, argv, sandboxEnv(sandboxEnviron(), got)); err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %s: %v\n", argv[0], err)
		return sandboxExitCannotStart
	}
	return 0 // not reached: the program has replaced this command
}

func sortedVars(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
