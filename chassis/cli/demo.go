package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	devpkg "github.com/loremlabs/thanks-computer/chassis/cli/dev"
)

// runDemo boots a local chassis and opens the txcl demo in a
// browser. It's the one-command "just let me try txcl" entry point:
// `brew install txco && txco demo`.
//
// Unlike `txco dev` it needs no workspace (runs from anywhere, in a temp
// data dir), spawns no apps, and doesn't watch — it just boots, serves
// the embedded /demo/ UI (already wired into the admin server), and
// opens the browser.
//
// It runs in a PRODUCTION RESPONSE POSTURE: TXCO_DEBUG_PRIVATE and
// TXCO_DEBUG_BREAKPOINTS off, so responses (and the demo's
// copy-as-curl) look like production — private `_txc` fields stripped.
// TRACE_MODE stays "full" (startChassis's default), so the demo's
// trace + per-op panes still get complete in/out (trace capture is
// independent of DEBUG_PRIVATE). This is a local learning tool, not a
// hardened deployment: admin auth stays open (as in dev) so the
// demo's apply/fire/trace calls work.
func runDemo(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("demo", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	adminAddr := fs.String("admin-addr", "", "admin API listen addr (default: an auto-picked free port)")
	webAddr := fs.String("web-addr", "", "web inlet listen addr (default: an auto-picked free port)")
	noBrowser := fs.Bool("no-browser", false, "don't open a browser; just print the URL")
	verbose := fs.Bool("verbose", false, "verbose logs from the spawned chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco demo [flags]

Boot a local chassis and open the txcl demo in your browser. No
workspace needed — run it from anywhere. Runs in a production response
posture (private `+"`_txc`"+` fields stripped from responses) while keeping full
execution traces for the demo. Picks free ports so it coexists with a
running `+"`txco dev`"+`.

Ctrl-C stops the chassis.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	banner.PrintLogo(stdout)

	// Production response posture, so demo output + copy-as-curl
	// look like production:
	//   - DEBUG_PRIVATE=false   → don't stamp _txc.flag_private
	//   - DEBUG_BREAKPOINTS=false → no breakpoint hooks
	//   - WEB_DEBUG=HIDE_PRIVATE_VARS → strip `_`-prefixed fields from the
	//     response body. Needed because the spawned chassis runs in the
	//     "dev" environment, which otherwise auto-adds SHOW_PRIVATE_VARS
	//     (chassis/config/config.go ~L392); the explicit HIDE flag wins.
	// These flow to the child via os.Environ(); startChassis's dev
	// defaults are set-if-missing so they don't clobber these. TRACE_MODE
	// is left untouched → stays "full", so the demo's trace view
	// keeps complete in/out (trace capture is independent of these).
	_ = os.Setenv("TXCO_DEBUG_PRIVATE", "false")
	_ = os.Setenv("TXCO_DEBUG_BREAKPOINTS", "false")
	_ = os.Setenv("TXCO_WEB_DEBUG", "HIDE_PRIVATE_VARS")
	// The demo talks to the admin API UNAUTHENTICATED — it's a
	// local, single-user, loopback tool. Force open admin auth so it
	// works even if the user's shell exports TXCO_ADMIN_USER /
	// TXCO_ADMIN_PASS / TXCO_AUTH_MODE from other work (which the
	// spawned chassis would otherwise inherit and then 401 the
	// demo's createDraft/fire). Open = mode "both" with empty
	// basic creds (chassis/auth/middleware.go: openDevContext).
	_ = os.Setenv("TXCO_AUTH_MODE", "both")
	_ = os.Setenv("TXCO_ADMIN_USER", "")
	_ = os.Setenv("TXCO_ADMIN_PASS", "")
	// Write traces synchronously so the demo's trace view is fully
	// populated the instant the response returns (dev defaults to async,
	// which races the UI's trace fetch → partial/empty trace). Fine for a
	// local single-user tool.
	_ = os.Setenv("TXCO_TRACE_ASYNC", "false")
	// Modest per-invocation wall-clock cap bump for compute ops in the
	// demo. The prod default (250 ms) is tight for users editing the
	// JS textarea to experiment (javy's QuickJS sandbox has no setTimeout
	// and `Date.now()` is per-invocation frozen, so there's no "free async
	// wait" — any extra time is real CPU). 500 ms gives some headroom on
	// slower machines without inviting abuse on a local single-user tool.
	_ = os.Setenv("TXCO_COMPUTE_MAX_WALL", "500ms")

	// Pick free ports unless overridden, so `txco demo` coexists with a
	// running `txco dev` (which owns :8081/:8080).
	aAddr := *adminAddr
	if aAddr == "" {
		p, err := freeAddr()
		if err != nil {
			fmt.Fprintf(stderr, "demo: pick admin port: %v\n", err)
			return 1
		}
		aAddr = p
	}
	wAddr := *webAddr
	if wAddr == "" {
		p, err := freeAddr()
		if err != nil {
			fmt.Fprintf(stderr, "demo: pick web port: %v\n", err)
			return 1
		}
		wAddr = p
	}

	// Temp data dir so we run from anywhere and litter nothing.
	// startChassis drops its .txco/dev/ tree under here; removed on exit.
	tmp, err := os.MkdirTemp("", "txco-demo-")
	if err != nil {
		fmt.Fprintf(stderr, "demo: temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var started []*devpkg.Process
	teardown := func() {
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop(5 * time.Second)
		}
	}
	defer teardown()

	// Catch a signal during startup too; don't cancel ctx here (that
	// would SIGKILL the immediate child and orphan its group) — let the
	// deferred teardown SIGTERM the whole process group first.
	abort := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			close(abort)
		case <-ctx.Done():
		}
	}()

	var chassisProc *devpkg.Process
	adminURL, webURL, err := startChassis(ctx, tmp, aAddr, wAddr, false, *verbose, stdout, stderr, &started, &chassisProc)
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}

	demoURL := strings.TrimRight(adminURL, "/") + "/demo/"
	fmt.Fprintf(stdout, "[txco] demo running (Ctrl-C to stop).\n")
	fmt.Fprintf(stdout, "[txco]   demo:      %s\n", demoURL)
	fmt.Fprintf(stdout, "[txco]   web inlet:  %s   (where your ops serve)\n", webURL)
	fmt.Fprintf(stdout, "[txco]   admin API:  %s\n", adminURL)

	if *noBrowser {
		fmt.Fprintf(stdout, "[txco] open %s in your browser.\n", demoURL)
	} else if berr := openBrowser(demoURL); berr != nil {
		fmt.Fprintf(stderr, "[txco] couldn't open a browser (%v); open %s yourself.\n", berr, demoURL)
	} else {
		fmt.Fprintf(stdout, "[txco] opened %s\n", demoURL)
	}

	select {
	case <-abort:
		fmt.Fprintln(stdout, "[txco] signal received; tearing down")
	case <-childExit(started):
		fmt.Fprintln(stderr, "[txco] chassis exited; tearing down")
	}
	return 0
}

// freeAddr returns a currently-free local TCP port as ":<port>".
// (Small TOCTOU window between probe and the chassis re-binding — fine
// for a local tool; the chassis still fail-fasts if the port is taken.)
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", err
	}
	return ":" + port, nil
}

// openBrowser shells out to the platform-native "open this URL" command
// (copy of chassis/cli/auth.openBrowser — that one is unexported). Fails
// cleanly on headless boxes; the caller falls back to printing the URL.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", u)
	default:
		return fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
