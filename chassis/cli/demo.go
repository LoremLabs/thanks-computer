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
	demopkg "github.com/loremlabs/thanks-computer/chassis/demo"
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
	// Breakpoints ON for the demo: the UI's "Open ↗" / "break at" select
	// builds `?_txc.break=<stack>/<scope>` URLs that only have any effect
	// when this flag is set. `txco demo` is local-loopback-only by
	// definition; the production-safety warning on this config field
	// ("NEVER enable in production") doesn't apply here.
	_ = os.Setenv("TXCO_DEBUG_BREAKPOINTS", "true")
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
	// Register the demo execution-hop endpoints (/v1/demo/*). Their
	// presence is the signal the admin-ui's probeDemoMode uses to
	// auto-route to #demo; a plain chassis / `txco dev` leaves this
	// unset so it lands on the normal admin interface instead.
	_ = os.Setenv("TXCO_DEMO_MODE", "true")

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
	adminURL, webURL, err := startChassis(ctx, tmp, aAddr, wAddr, false, false, *verbose, stdout, stderr, &started, &chassisProc)
	if err != nil {
		fmt.Fprintf(stderr, "demo: %v\n", err)
		return 1
	}

	// startChassis only waits for admin's /healthz. The chassis brings
	// the web inlet up in its own goroutine (personality/web/web.go),
	// so its listener can lag admin's by a few hundred ms on cold-boot.
	// The demo SPA opens with a parallel-fire storm — Runner.onMount
	// seeds ~15 stacks, each ending in a bindHostname, and then
	// auto-fires the first step against <stack>.local.thanks.computer.
	// If the web inlet's listener isn't ready when that fire lands, the
	// browser sees a connection refused or 404 before the chassis has
	// chance to register the hostname route. Probe both heads here so
	// the URL we open is fully warm.
	//
	// Best-effort: a failed probe warns but still opens the browser —
	// the URL is already printed and the user can reload if the chassis
	// is genuinely stuck. 10s budget at 200ms intervals is generous on
	// loopback (warm chassis answers within one tick).
	if err := devpkg.WaitHealthy(ctx, webURL+"/healthz", 10*time.Second, 200*time.Millisecond); err != nil {
		fmt.Fprintf(stderr, "[txco] warn: web inlet readiness: %v\n", err)
	}
	// /v1/demo/info exercises the protected subrouter's auth path
	// (open in demo mode via openDevContext) — confirms the demo
	// handlers are wired AND that auth lets the SPA's first call
	// through. probeDemoMode in admin-ui hits this exact endpoint on
	// first paint to decide whether to auto-route to #demo.
	if err := devpkg.WaitHealthy(ctx, strings.TrimRight(adminURL, "/")+"/v1/demo/info", 10*time.Second, 200*time.Millisecond); err != nil {
		fmt.Fprintf(stderr, "[txco] warn: demo endpoint readiness: %v\n", err)
	}

	// Pre-seed every walkthrough step's stack BEFORE opening the
	// browser. Until this hop existed the SPA's onMount did the
	// seeding itself — which meant the user could see a partial-seed
	// error message on first load if any of the ~15 admin write
	// chains hit transient SQLite contention. Moving the seed here
	// (Go, serial, on loopback) eliminates that class of error: the
	// SPA opens to a chassis that already has every demo stack
	// active with its hostname bound. The SPA's listStacks filter
	// then sees them all and does nothing on mount.
	//
	// Best-effort: a seed failure logs to stderr but still opens the
	// browser. The SPA falls back to its own onMount seed for any
	// missing stack — same self-healing path that handles a chassis
	// restart with stale state.
	fmt.Fprintf(stdout, "[txco] seeding walkthrough curriculum...\n")
	seedStart := time.Now()
	if err := demopkg.Seed(ctx, adminURL); err != nil {
		fmt.Fprintf(stderr, "[txco] warn: curriculum seed had failures (SPA will retry):\n%v\n", err)
	} else {
		fmt.Fprintf(stdout, "[txco] curriculum seeded (%s)\n", time.Since(seedStart).Round(time.Millisecond))
	}

	// The demo SPA lives inside admin-ui as the #demo route. The chassis
	// also serves a transitional /demo/ redirect (server.go) for older
	// bookmarks and CLI shims, but new processes land users directly on
	// the merged URL. probeDemoMode in the admin-ui store sees
	// /v1/demo/info and auto-routes to #demo on first load.
	demoURL := strings.TrimRight(adminURL, "/") + "/admin/#demo"
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
