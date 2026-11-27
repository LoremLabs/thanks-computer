package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/workspace/local"
)

// `txco attach` makes this computer a workspace of another chassis.
//
//	txco attach <site>      confirm in the browser (attach_browser.go), then
//	                        as with a code
//	txco attach <code>      enroll this computer for the workspace the code
//	                        is for, working in the CURRENT directory, then
//	                        serve it
//	txco attach             serve every workspace this computer holds, but
//	                        a while-running one, which only the process that
//	                        made it serves
//	txco attach list        say what this computer holds, and where
//
// It runs the txco binary as a small chassis with a fixed profile: one
// personality, the link, which dials out; a processor; the built-in
// `_attach` stack; and the owner workspace provider, which runs commands as
// you, in the folder you attached from. Nothing listens.
//
// The folder is taken ONCE, from the directory `txco attach <code>` is run
// in, and kept in the credential's file. Running `txco attach` again later,
// from anywhere, serves each workspace from the folder it was enrolled in.
// To move a workspace to another folder, ask for a new code and enroll it
// there.

func attachHome() (home.Home, error) {
	dir, err := auth.HomePath()
	if err != nil {
		return home.Home{}, err
	}
	h := home.Home{Dir: filepath.Join(dir, "attach")}
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return home.Home{}, err
	}
	return h, nil
}

// runAttach handles the command. serve reports that the process should go
// on to boot the chassis (the caller falls through to the server); the
// profile for it is already in place.
func runAttach(args []string, stdin io.Reader, stdout, stderr io.Writer) (status int, serve bool) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	address := fs.String("address", "", "the chassis to attach to, when the code does not carry it (e.g. https://attach.example.com)")
	yes := fs.Bool("yes", false, "attach without asking")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: txco attach [--address URL] [--yes] [<site> | <code> | list]")
		fmt.Fprintln(stderr, "\nRun it in the folder the work should happen in.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2, false
	}
	h, err := attachHome()
	if err != nil {
		fmt.Fprintln(stderr, "txco attach:", err)
		return 1, false
	}
	switch {
	case fs.NArg() > 1:
		fs.Usage()
		return 2, false
	case fs.Arg(0) == "list":
		return attachList(h, stdout, stderr), false
	case fs.NArg() == 1 && *address == "" && isSite(fs.Arg(0)):
		if err := attachBrowser(h, fs.Arg(0), *yes, stdin, stdout); err != nil {
			fmt.Fprintln(stderr, "txco attach:", err)
			return 1, false
		}
	case fs.NArg() == 1:
		if err := attachEnroll(h, fs.Arg(0), *address, *yes, stdin, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "txco attach:", err)
			return 1, false
		}
	}
	creds, err := h.Load()
	if err != nil {
		fmt.Fprintln(stderr, "txco attach:", err)
		return 1, false
	}
	// A while-running share is served only by the process that made it.
	// One nobody holds was left by a process that died: serving goes on so
	// that it is cleared away (attachlink).
	live := 0
	for _, c := range creds {
		if c.Ended == "" && (!c.WhileRunning || h.Holds(c.ID) || h.Left(c.ID)) {
			live++
		}
	}
	if live == 0 {
		fmt.Fprintln(stderr, "txco attach: this computer holds no attached workspace. Run `txco attach <code>` in the folder the work should happen in.")
		return 1, false
	}
	if err := attachProfile(h); err != nil {
		fmt.Fprintln(stderr, "txco attach:", err)
		return 1, false
	}
	return 0, true
}

func attachList(h home.Home, stdout, stderr io.Writer) int {
	creds, err := h.Load()
	if err != nil {
		fmt.Fprintln(stderr, "txco attach:", err)
		return 1
	}
	if len(creds) == 0 {
		fmt.Fprintln(stdout, "This computer holds no attached workspace.")
		return 0
	}
	for _, c := range creds {
		what := c.Label
		if what == "" {
			what = c.Name
		}
		state := ""
		switch {
		case c.Ended != "":
			state = "  (ended: " + c.Ended + ")"
		case c.WhileRunning && h.Left(c.ID):
			state = "  (ended: stopped)"
		case c.WhileRunning:
			state = "  (while its txco attach runs)"
		}
		fmt.Fprintf(stdout, "%s · %s · %s%s\n", what, home.Pretty(c.Root), c.Address, state)
	}
	return 0
}

// attachEnroll spends a code: it asks the chassis what the code is for,
// asks the owner, makes a key, and enrolls it in a request signed with that
// key. The folder recorded is the current directory.
func attachEnroll(h home.Home, pasted, address string, yes bool, stdin io.Reader, stdout, stderr io.Writer) error {
	code, base := attached.SplitCode(pasted)
	if address != "" {
		base = strings.TrimRight(address, "/")
	}
	if base == "" {
		return fmt.Errorf("the code does not say where to attach; pass --address https://…")
	}
	root, err := attachRoot()
	if err != nil {
		return err
	}
	var ask func() bool
	if !yes {
		if !auth.StdinIsTTY() {
			return fmt.Errorf("not a terminal: pass --yes to attach without asking")
		}
		in := bufio.NewReader(stdin)
		ask = func() bool {
			fmt.Fprint(stdout, "Attach? [y/N] ")
			line, _ := in.ReadString('\n')
			return isYes(line)
		}
	}
	return spendCode(h, code, base, root, false, ask, stdout)
}

// attachRoot is the folder work starts in: the current directory, its
// links resolved.
func attachRoot() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	return root, nil
}

func isYes(line string) bool {
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// spendCode enrolls this computer, working in root, with the code for base.
// ask, when not nil, is first told what the code is for and must say yes.
// whileRunning: the share ends when this process stops; it holds the
// credential's lock from before the file exists.
func spendCode(h home.Home, code, base, root string, whileRunning bool, ask func() bool, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if ask != nil {
		info, err := attached.PeekCode(ctx, nil, base, code)
		if err != nil {
			return err
		}
		what := info.Label
		if what == "" {
			what = "a workspace"
		}
		fmt.Fprintf(stdout, "Attach this computer to %s on %s,\nworking in %s?\n", what, strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"), home.Pretty(root))
		fmt.Fprintln(stdout, "Commands sent to it run as you, starting in that folder. It is not a sandbox.")
		if !ask() {
			return fmt.Errorf("not attached")
		}
	}

	// A key of this attachment's own, kept apart from `txco auth`'s. Its
	// private half never leaves the credential's file.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	machine, _ := os.Hostname()
	out, err := attached.EnrollKey(ctx, nil, base, key, attached.EnrollBody{
		Code: code, OS: runtime.GOOS, Root: home.Pretty(root), Machine: machine,
	})
	if err != nil {
		return err
	}
	if whileRunning {
		if err := h.Hold(out.Credential); err != nil {
			return fmt.Errorf("enrolled, but the credential could not be kept (ask for a new code): %w", err)
		}
	}
	if err := h.Save(home.Credential{
		ID: out.Credential, Address: base, Name: out.Name, Label: out.Label,
		Root: root, Seed: key.Seed(), Machine: machine, CreatedAt: time.Now().UTC(),
		WhileRunning: whileRunning,
	}); err != nil {
		h.Release(out.Credential)
		return fmt.Errorf("enrolled, but the credential could not be saved (ask for a new code): %w", err)
	}
	fmt.Fprintf(stdout, "Enrolled. Attached: %s · %s\n", machine, home.Pretty(root))
	return nil
}

// attachProfile fixes the configuration of the small chassis `txco attach`
// runs. It is the WHOLE configuration: every TXCO_* setting in the
// environment is cleared first (TXCO_HOME, which located the folder, aside),
// so settings meant for the owner's own dev chassis turn nothing on here.
//
// The commands this chassis runs still start from the environment `txco
// attach` was started with, untouched: it is handed to the owner provider
// before anything is cleared.
func attachProfile(h home.Home) error {
	local.SetOwnerEnv(os.Environ())
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "TXCO_") && name != "TXCO_HOME" {
			_ = os.Unsetenv(name)
		}
	}
	// The chassis's own stores. Every one of its data paths defaults to
	// ./chassis/data/… under the working directory, so the working
	// directory is made the attach folder: all of them land in
	// <attach>/chassis/, including any a later chassis adds, and none in
	// the directory the command happened to be run in. Commands are not
	// affected: each is started in its workspace's own folder, by name.
	if err := os.MkdirAll(h.ChassisDir(), 0o700); err != nil {
		return err
	}
	if err := os.Chdir(h.Dir); err != nil {
		return err
	}
	for k, v := range map[string]string{
		// One personality, and it listens to nothing.
		"TXCO_PERSONALITIES": "attachlink",
		"TXCO_ATTACH_HOME":   h.Dir,
		// Commands run as the owner, and only for the _attach stack.
		"TXCO_WORKSPACE_PROVIDER": local.OwnerName,
		// The owner's own rules for _attach, over the built-in ones.
		"TXCO_SYSTEM_OPSTACKS_DIR": h.OpsParent(),
		// A request's timeout is the other chassis's to choose; a detached
		// job's is its whole life.
		"TXCO_OP_TIMEOUT_MAX":            "24h",
		"TXCO_WORKSPACE_DEFAULT_TIMEOUT": "10m",
		// Nothing about a run is recorded or sent anywhere.
		"TXCO_TRACE_MODE":        "off",
		"TXCO_USAGE_ENABLED":     "false",
		"TXCO_TELEMETRY_ENABLED": "false",
		"TXCO_LOG_OPS":           "disabled",
		"TXCO_LOG_LEVEL":         "warn",
		"TXCO_ENV":               "attach",
	} {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	// The server reads its settings from the environment above; the
	// command line carries none.
	os.Args = []string{os.Args[0], "serve"}
	return nil
}
