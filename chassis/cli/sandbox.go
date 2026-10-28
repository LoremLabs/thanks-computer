package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// `txco sandbox` hands a program what named sandboxes hold, asked for with
// the run grant this command was itself started with: their variables in
// the program's environment, and their files served where the program
// reads them.
//
//	txco sandbox github -- gh pr list
//	txco sandbox github postgres -- ./migrate
//	txco sandbox serve --pid /var/lib/txco/run.pid github
//
// It runs inside a workspace, started by a rule's `workspace://…/exec WITH
// grant`. A sandbox is a declaration of the stack (SANDBOXES/<name>.yaml):
// the program names the sandbox and never a secret. Each value the sandbox
// names is one request to the chassis, decided like any other made with
// the grant — by the tenant's `_grant` stack, starting from the chassis's
// proposal — and leaves the same trace. The chassis is reached over its
// socket on this machine (TXCO_GRANT_SOCK) or, from another machine, over
// HTTPS at its URL (--parent, else TXCO_PARENT_URL).
//
// Variables are asked for once, when the program starts. Files are served
// as named pipes (sandbox_files.go): each read is asked for at that moment.
// With files, this command stays the program's parent — it serves them
// while the program runs, forwards its signals and exits with its code —
// and removes them after. `serve` serves the files alone, in the
// background, until it is stopped or the run's grant ends.

// The launcher's own exit codes, from sysexits.h and the shell. Once the
// program is running, every code is the program's.
const (
	sandboxExitUsage       = 2   // the command line
	sandboxExitData        = 65  // EX_DATAERR: a value that no variable can hold
	sandboxExitUnavailable = 69  // EX_UNAVAILABLE: the chassis could not be asked, or could not decide
	sandboxExitCantCreate  = 73  // EX_CANTCREAT: a file to serve could not be made
	sandboxExitRefused     = 77  // EX_NOPERM: the chassis decided, and the answer is no
	sandboxExitCannotStart = 126 // the program was found and could not be started
	sandboxExitNotFound    = 127 // the program was not found
)

const sandboxUsage = `Usage:
  txco sandbox [--parent URL] [--dir DIR] NAME [NAME…] -- PROGRAM [ARGS…]
  txco sandbox serve [--parent URL] [--dir DIR] [--pid FILE] NAME [NAME…]

Start PROGRAM inside the named sandboxes. Each NAME is a sandbox the stack
declares (SANDBOXES/NAME.yaml) and the run grant this command was started
with may open. Its variables are put in PROGRAM's environment; its files
(and its dotenv file) are served where PROGRAM reads them, while it runs,
and removed after. A sandbox opens whole or not at all.

PROGRAM is given what the sandboxes set and none of the grant's own
variables, so it cannot open more. Its exit code and signals are its own.

serve serves the sandboxes' files without a program: in the background,
until SIGTERM or SIGINT, or until the run's grant ends. --pid writes its
process id to FILE (and removes it on exit).

Files are named pipes, mode 0600: each read asks the chassis for the value
at that moment, and is decided and logged there. A path is under DIR (the
current directory by default), or under the home directory for ~/.

  --parent URL  reach the chassis over HTTPS at URL (default:
                $TXCO_PARENT_URL), when it has no socket on this machine
  --dir DIR     serve relative paths under DIR (default: the current
                directory)
  --pid FILE    (serve) write the process id to FILE

Exit codes, before PROGRAM starts:
   2  the command line
  65  a value holds a byte no variable can (a NUL)
  69  the chassis could not be asked, or could not decide
  73  a file to serve could not be made
  77  a sandbox was refused
 126  PROGRAM could not be started
 127  PROGRAM was not found

Runs inside a workspace, from a rule's "workspace://<name>/exec" WITH grant.
`

// sandboxOptions are the launcher's own flags.
type sandboxOptions struct {
	parent string
	dir    string
	pid    string
}

// parseSandbox reads the command line: the launcher's flags, the
// sandboxes, then `--`, then the program. The `--` is required: a
// program's name ("git", "make") is as good a sandbox name as any, so
// nothing else could tell them apart. It fails on anything it does not
// understand — a flag that was meant for the launcher and reached the
// program instead would be a surprise. serve takes no program.
func parseSandbox(args []string, serve bool) (names, argv []string, opts sandboxOptions, help bool, err error) {
	seen := map[string]bool{}
	value := func(i int, flag string) (string, error) {
		if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") && args[i+1] != "-" {
			return "", fmt.Errorf("%s needs a value", flag)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--" && !serve:
			argv = args[i+1:]
			if len(argv) == 0 {
				return nil, nil, opts, false, errors.New("no program to start after --")
			}
			if len(names) == 0 {
				return nil, nil, opts, false, errors.New("no sandbox named before --")
			}
			return names, argv, opts, false, nil
		case a == "-h" || a == "--help" || a == "help":
			return nil, nil, opts, true, nil
		case a == "--parent" || a == "--dir" || (a == "--pid" && serve):
			v, verr := value(i, a)
			if verr != nil {
				return nil, nil, opts, false, verr
			}
			switch a {
			case "--parent":
				opts.parent = v
			case "--dir":
				opts.dir = v
			default:
				opts.pid = v
			}
			i++
		case strings.HasPrefix(a, "-"):
			if serve {
				return nil, nil, opts, false, fmt.Errorf("unknown flag %s", a)
			}
			return nil, nil, opts, false, fmt.Errorf("unknown flag %s (put -- before the program's own flags)", a)
		case !sandbox.ValidName(a):
			if serve {
				return nil, nil, opts, false, fmt.Errorf("%q is not a sandbox name (a name matching [a-z][a-z0-9_-]*)", a)
			}
			return nil, nil, opts, false, fmt.Errorf("%q is not a sandbox name (a name matching [a-z][a-z0-9_-]*): the program goes after --", a)
		case !seen[a]:
			seen[a] = true
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return nil, nil, opts, false, errors.New("no sandbox named")
	}
	if serve {
		return names, nil, opts, false, nil
	}
	return nil, nil, opts, false, errors.New("no program to start: put -- between the sandboxes and the program")
}

// sandboxEnv is the program's environment: this command's, less every
// variable of the grant, plus what the sandboxes set. A variable a sandbox
// sets replaces one of the same name that was already there.
func sandboxEnv(environ []string, got map[string]string) []string {
	drop := map[string]bool{}
	for _, k := range grantwire.Env {
		drop[k] = true
	}
	drop[envParentURL] = true
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

// envParentURL names the chassis's URL from another machine (a node's
// --parent-url): where the network form reaches it.
const envParentURL = "TXCO_PARENT_URL"

// Seams for tests.
var (
	// sandboxOpen asks the chassis at endpoint — a socket path, or an
	// http(s) URL — to open one entry of a sandbox.
	sandboxOpen = func(ctx context.Context, endpoint string, req grantwire.Request) (grantwire.Opened, error) {
		if strings.HasPrefix(endpoint, "https://") || strings.HasPrefix(endpoint, "http://") {
			return grantwire.OpenURL(ctx, nil, endpoint, req)
		}
		return grantwire.Open(ctx, endpoint, req)
	}
	sandboxExec    = syscall.Exec
	sandboxStart   = startChild
	sandboxLook    = exec.LookPath
	sandboxEnviron = os.Environ
	sandboxGetenv  = os.Getenv
)

// sandboxEndpoint is where the chassis is asked: --parent, else the
// socket on this machine, else TXCO_PARENT_URL. "" and why when none.
func sandboxEndpoint(opts sandboxOptions) (string, string) {
	if opts.parent != "" {
		return opts.parent, ""
	}
	if s := sandboxGetenv(grantwire.EnvSocket); s != "" {
		return s, ""
	}
	if u := sandboxGetenv(envParentURL); u != "" {
		return u, ""
	}
	return "", fmt.Sprintf("the chassis is not listening for the launcher (%s): add `grant` to its --personalities, or name its URL (--parent, %s)", grantwire.EnvSocket, envParentURL)
}

// sandboxDirs is where files are served: --dir (else the current
// directory), and the home directory for ~/.
func sandboxDirs(opts sandboxOptions) (string, string) {
	dir := opts.dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return dir, sandboxGetenv("HOME")
}

func runSandbox(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "serve" {
		return runSandboxServe(args[1:], stdout, stderr)
	}
	names, argv, opts, help, err := parseSandbox(args, false)
	if help {
		fmt.Fprint(stdout, sandboxUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %v\n\n%s", err, sandboxUsage)
		return sandboxExitUsage
	}
	token := sandboxGetenv(grantwire.EnvToken)
	if token == "" {
		fmt.Fprintf(stderr, "txco sandbox: no run grant in this environment (%s): start this command from a rule's exec WITH grant\n", grantwire.EnvToken)
		return sandboxExitUnavailable
	}
	endpoint, why := sandboxEndpoint(opts)
	if endpoint == "" {
		fmt.Fprintf(stderr, "txco sandbox: %s\n", why)
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
	serve := map[string][]string{}
	for _, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		opened, err := sandboxOpen(ctx, endpoint, grantwire.Request{Token: token, Sandbox: name})
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
		if len(opened.Serve) > 0 {
			serve[name] = opened.Serve
		}
		code := 0
		for _, v := range sortedVars(opened.Env) {
			value := opened.Env[v]
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
	env := sandboxEnv(sandboxEnviron(), got)
	if len(serve) == 0 {
		// Nothing to serve: PROGRAM replaces this command, as it always has.
		if err := sandboxExec(path, argv, env); err != nil {
			fmt.Fprintf(stderr, "txco sandbox: %s: %v\n", argv[0], err)
			return sandboxExitCannotStart
		}
		return 0 // not reached: the program has replaced this command
	}

	// Files: served while PROGRAM runs, by this command as its parent.
	dir, home := sandboxDirs(opts)
	open := func(ctx context.Context, req grantwire.Request) (grantwire.Opened, error) {
		return sandboxOpen(ctx, endpoint, req)
	}
	srv, err := newFileServer(open, token, stderr, dir, home, serve)
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %v\n", err)
		return sandboxExitUsage
	}
	if err := srv.prepare(); err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %v\n", err)
		return sandboxExitCantCreate
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.serve(ctx)
	code := sandboxStart(path, argv, env, stderr)
	cancel()
	srv.stop()
	return code
}

// startChild runs PROGRAM as this command's child — its standard streams
// this command's, every signal this command gets passed on — and answers
// its exit code (128 + the signal, when a signal ended it).
func startChild(path string, argv, env []string, stderr io.Writer) int {
	cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "txco sandbox: %s: %v\n", argv[0], err)
		return sandboxExitCannotStart
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		case <-done:
			if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return cmd.ProcessState.ExitCode()
		}
	}
}

// runSandboxServe serves the sandboxes' files until it is stopped or the
// run's grant ends.
func runSandboxServe(args []string, stdout, stderr io.Writer) int {
	names, _, opts, help, err := parseSandbox(args, true)
	if help {
		fmt.Fprint(stdout, sandboxUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox serve: %v\n\n%s", err, sandboxUsage)
		return sandboxExitUsage
	}
	token := sandboxGetenv(grantwire.EnvToken)
	if token == "" {
		fmt.Fprintf(stderr, "txco sandbox serve: no run grant in this environment (%s): start this command from a rule's exec WITH grant\n", grantwire.EnvToken)
		return sandboxExitUnavailable
	}
	endpoint, why := sandboxEndpoint(opts)
	if endpoint == "" {
		fmt.Fprintf(stderr, "txco sandbox serve: %s\n", why)
		return sandboxExitUnavailable
	}
	serve := map[string][]string{}
	for _, name := range names {
		// Which files: the paths only. Nothing is released until a read.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		opened, err := sandboxOpen(ctx, endpoint, grantwire.Request{Token: token, Sandbox: name, Entry: grantwire.EntryList})
		cancel()
		switch {
		case errors.Is(err, grantwire.ErrRefused):
			fmt.Fprintf(stderr, "txco sandbox serve: %s: refused\n", name)
			return sandboxExitRefused
		case err != nil:
			fmt.Fprintf(stderr, "txco sandbox serve: %s: %v\n", name, err)
			return sandboxExitUnavailable
		case len(opened.Serve) == 0:
			fmt.Fprintf(stderr, "txco sandbox serve: %s serves no file (its variables are handed to a program: txco sandbox %s -- PROGRAM)\n", name, name)
			return sandboxExitUsage
		}
		serve[name] = opened.Serve
	}
	if opts.pid != "" {
		if pid, live := pidFileLive(opts.pid); live {
			fmt.Fprintf(stderr, "txco sandbox serve: %s names a running process (%d): stop it first\n", opts.pid, pid)
			return sandboxExitUsage
		}
	}
	dir, home := sandboxDirs(opts)
	open := func(ctx context.Context, req grantwire.Request) (grantwire.Opened, error) {
		return sandboxOpen(ctx, endpoint, req)
	}
	srv, err := newFileServer(open, token, stderr, dir, home, serve)
	if err != nil {
		fmt.Fprintf(stderr, "txco sandbox serve: %v\n", err)
		return sandboxExitUsage
	}
	if err := srv.prepare(); err != nil {
		fmt.Fprintf(stderr, "txco sandbox serve: %v\n", err)
		return sandboxExitCantCreate
	}
	me := os.Getpid()
	if opts.pid != "" {
		if err := os.WriteFile(opts.pid, []byte(strconv.Itoa(me)+"\n"), 0o600); err != nil {
			srv.remove()
			fmt.Fprintf(stderr, "txco sandbox serve: %s: %v\n", opts.pid, err)
			return sandboxExitCantCreate
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	srv.serve(ctx)
	fmt.Fprintf(stderr, "%s txco sandbox serve: serving %d files of %s (pid %d)\n", time.Now().UTC().Format(time.RFC3339), len(srv.files), strings.Join(names, ", "), me)
	why = "stopped"
	select {
	case <-ctx.Done():
	case <-srv.Gone():
		why = "the run's grant has ended"
	}
	cancel()
	srv.stop()
	if opts.pid != "" {
		if pid, _ := pidFileLive(opts.pid); pid == me {
			_ = os.Remove(opts.pid)
		}
	}
	fmt.Fprintf(stderr, "%s txco sandbox serve: %s; the files are removed\n", time.Now().UTC().Format(time.RFC3339), why)
	return 0
}

// pidFileLive reads a pid file: the process id in it, and whether that
// process is running.
func pidFileLive(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, syscall.Kill(pid, 0) == nil
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
