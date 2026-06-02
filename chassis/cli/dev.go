package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	devpkg "github.com/loremlabs/thanks-computer/chassis/cli/dev"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
	"github.com/loremlabs/thanks-computer/chassis/sysops"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

const adminUIDevPort = ":6161"

// devDNSListenAddr is the loopback address `txco dev --dns` binds the
// authoritative-DNS head on (UDP+TCP). Deliberately NOT :5353 — that's
// mDNS on macOS and would clash. dig with `-p 5354`.
const devDNSListenAddr = "127.0.0.1:5354"

// runDev orchestrates the developer dev loop:
//
//  1. Load + validate workspace config (txco.yaml).
//  2. Spawn each declared app (apps come up first so the chassis sees
//     healthy targets when it starts dispatching).
//  3. Health-check apps; tear down on any failure.
//  4. Spawn the chassis subprocess (per-workspace temp DB, picks a free
//     admin port if the configured one is taken).
//  5. Walk OPS/, resolve op:// references, strip mock_res when policy
//     denies, and POST the bundle to the spawned chassis.
//  6. Watch OPS/ for *.txcl / *.json changes; re-apply on debounced
//     change.
//  7. Tear down on Ctrl-C: chassis first (stop accepting events), then
//     apps; SIGTERM with 5s grace, SIGKILL stragglers.
func runDev(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("dev", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml (default: the config's `target:`, or `dev`)")
	noChassis := fs.Bool("no-chassis", false, "skip spawning a chassis subprocess; assume one is already running at the target's chassis URL")
	chassisAddr := fs.String("chassis-addr", "", "override the spawned chassis admin listen addr (e.g. \":8088\")")
	webAddr := fs.String("web-addr", "", "override the spawned chassis web inlet listen addr (e.g. \":8090\"). Lets you run side-by-side dev instances without port clashes.")
	workspace := fs.String("workspace", ".", "workspace root (defaults to cwd)")
	uiDev := fs.Bool("ui", false, "also start the admin-ui Vite dev server on "+adminUIDevPort+" (HMR; opens admin-ui/ found by walking up from the workspace)")
	tcpHead := fs.Bool("tcp", false, "start the TCP head (binds :5050). Disabled by default — most workflows only need web + cron + admin.")
	dnsHead := fs.Bool("dns", false, "start the authoritative-DNS head with dev defaults: binds "+devDNSListenAddr+" (UDP+TCP) and pre-sets synthesis infra (nameservers ns1/ns2.localhost, edge 127.0.0.1, MX localhost) so a delegated zone resolves out of the box. Disabled by default. Override any of TXCO_DNS_NAMESERVERS/EDGE_IPS/MX_HOST.")
	watch := fs.Bool("watch", true, "watch sources and hot-reload: compute edits rebuild + reactivate; OPS edits push to a per-stack draft. On by default (that's what `dev` is for); pass --watch=false to disable.")
	apply := fs.Bool("apply", true, "push local OPS/ + computes and activate on startup (manifest-aware; skips stacks already in sync). On by default; pass --apply=false to leave chassis state untouched (e.g. when iterating via the admin UI).")
	forceOpstacks := fs.Bool("force-opstacks", false, "overwrite an existing opstacks/ with the embedded system-opstack template (default: scaffold only if opstacks/ is absent)")
	verbose := fs.Bool("verbose", false, "verbose logs from the spawned chassis (TXCO_LOG_LEVEL=debug). Default INFO. A parent TXCO_LOG_LEVEL still wins unless --verbose is set.")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco dev [flags]

Start the developer dev loop: spawn each `+"`apps:`"+` entry from
txco.yaml, wait for their health checks, spawn a chassis, apply the
local OPS/ bundle, and watch for source changes.

Ctrl-C tears everything down cleanly.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	banner.PrintLogo(stdout)

	dir, err := resolveDir(*workspace)
	if err != nil {
		fmt.Fprintf(stderr, "dev: resolve workspace: %v\n", err)
		return 1
	}

	// txco.yaml is optional — a colocated-compute workspace needs none.
	// resolveFullTarget synthesizes a local "dev" target when no config exists
	// (same as `apply`). When a config with explicit targets is present,
	// validate the selected target.
	cfg := loadWorkspaceConfig(dir)
	if cfg == nil {
		cfg = &workspaceConfig{} // no txco.yaml: empty apps/targets, all defaults
	}
	resolved := resolveFullTarget(dir, *target)
	if len(cfg.Targets) > 0 {
		if _, ok := cfg.Targets[resolved.Name]; !ok {
			fmt.Fprintf(stderr, "dev: target %q not found in txco.yaml (declared: %s)\n",
				resolved.Name, strings.Join(targetNames(cfg.Targets), ", "))
			return 1
		}
	}

	// Validate the bundle's op:// refs against the resolved ops map
	// before spawning anything — fail-fast with a helpful message.
	ops, err := bundle.Walk(dir)
	if err != nil {
		fmt.Fprintf(stderr, "dev: walk %s: %v\n", dir, err)
		return 1
	}
	// Pre-flight: resolve op://NAME per resonator (colocated <name>.js wins, else the
	// txco.yaml URL). Builds colocated computes (cached) so compile/build errors
	// surface here, before any chassis is spawned.
	if _, _, cerr := resolveOpRefsColocated(ops, buildOpRefMap(resolved), dir, stderr); cerr != nil {
		fmt.Fprintf(stderr, "dev: %v\n", cerr)
		return 1
	}

	// txco dev writes some transient state into the workspace under
	// .txco/. Auto-add that to .gitignore so a fresh `git status`
	// after `txco dev` doesn't surprise anyone with chassis state.
	// `*.wasm` covers the named build artifacts `txco op build` drops next
	// to a compute's source (the content-addressed module is uploaded; the
	// local .wasm is a disposable build output).
	for _, entry := range []string{".txco/", "*.wasm"} {
		if added, err := ensureGitignored(dir, entry); err == nil && added {
			fmt.Fprintf(stdout, "[txco] added %s to .gitignore\n", entry)
		}
	}

	// Scaffold the editable system-opstack bundle (_sys/boot, …) from
	// the binary so the routing pipeline is visible and editable in
	// the workspace instead of an invisible default. No-clobber:
	// existing opstacks/ is left alone unless --force-opstacks. The
	// chassis hot-reloads this dir in dev (TXCO_SYSTEM_OPSTACKS_WATCH).
	if wrote, serr := sysops.Scaffold(dir, *forceOpstacks); serr != nil {
		fmt.Fprintf(stderr, "dev: scaffold OPS/_sys: %v\n", serr)
		return 1
	} else if wrote {
		fmt.Fprintf(stdout, "[txco] scaffolded OPS/_sys (editable _sys/boot router; edits hot-reload)\n")
	}

	// Lifecycle wiring: a context that gets canceled on signal,
	// plus a slice of started processes we tear down in reverse.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var started []*devpkg.Process
	teardown := func() {
		// Tear down in reverse declaration order: chassis (last started)
		// first so it stops accepting events before apps die.
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop(5 * time.Second)
		}
	}
	defer teardown()

	// Catch a signal during startup as well — tear down whatever has
	// been spawned so far and bail.
	//
	// Deliberately NOT canceling ctx here. The children were started
	// with exec.CommandContext(ctx); cancelling would trigger
	// os/exec's built-in SIGKILL on each immediate child (sh) — which
	// orphans the grandchildren (pnpm, vite) in the same process
	// group. Letting `defer teardown()` run first means Stop() can
	// SIGTERM the whole pgid, taking the grandchildren with it. The
	// final `defer cancel()` still runs to release resources.
	abort := make(chan struct{})
	go func() {
		select {
		case <-sigCh:
			close(abort)
		case <-ctx.Done():
		}
	}()

	// 2. Spawn apps in declared order.
	appNames := orderedAppNames(cfg.Apps)
	for _, name := range appNames {
		app := cfg.Apps[name]
		if app.Start == "" {
			fmt.Fprintf(stderr, "dev: app %q has no start command\n", name)
			return 1
		}
		appDir := dir
		if app.Path != "" {
			appDir = filepath.Join(dir, app.Path)
		}
		fmt.Fprintf(stdout, "[txco] starting app %q (%s)\n", name, app.Start)
		p, err := devpkg.Spawn(ctx, devpkg.SpawnConfig{
			Name: name,
			Dir:  appDir,
			Cmd:  app.Start,
			Out:  stdout,
		})
		if err != nil {
			fmt.Fprintf(stderr, "dev: spawn app %q: %v\n", name, err)
			return 1
		}
		started = append(started, p)
	}

	// 3. Health-check apps.
	for _, name := range appNames {
		app := cfg.Apps[name]
		if app.Health == "" {
			fmt.Fprintf(stdout, "[txco] no health URL for %q; skipping check\n", name)
			continue
		}
		fmt.Fprintf(stdout, "[txco] waiting for %q health (%s)\n", name, app.Health)
		if err := devpkg.WaitHealthy(ctx, app.Health, 60*time.Second, time.Second); err != nil {
			fmt.Fprintf(stderr, "dev: %v\n", err)
			return 1
		}
	}

	// 4. Spawn the chassis subprocess.
	var chassisProc *devpkg.Process
	chassisURL := resolved.Chassis
	webURL := "" // unknown when --no-chassis (assume caller knows where to curl)
	if !*noChassis {
		chassisURL, webURL, err = startChassis(ctx, dir, *chassisAddr, *webAddr, *tcpHead, *dnsHead, *verbose, stdout, stderr, &started, &chassisProc)
		if err != nil {
			fmt.Fprintf(stderr, "dev: %v\n", err)
			return 1
		}
		// Override the resolved target's chassis URL so apply/diff
		// inside this process talks to the spawned chassis.
		resolved.Chassis = chassisURL
	}

	// Forward drain signals to the chassis child. `txco dev` is a
	// supervisor; the chassis runs in its own process group, so an
	// operator who sends SIGUSR1/SIGUSR2 to the dev process expects it to
	// reach the chassis (drain on / resume), not be swallowed by the
	// wrapper. We forward ONLY to the chassis group — never to app
	// children, where SIGUSR1 has its own meaning (Node, for one, starts
	// its inspector on SIGUSR1). INT/TERM stay on the teardown path above.
	if chassisProc != nil {
		drainCh := make(chan os.Signal, 2)
		signal.Notify(drainCh, syscall.SIGUSR1, syscall.SIGUSR2)
		go func() {
			defer signal.Stop(drainCh)
			for {
				select {
				case sig := <-drainCh:
					ssig, ok := sig.(syscall.Signal)
					if !ok {
						continue
					}
					if err := chassisProc.Signal(ssig); err != nil {
						fmt.Fprintf(stderr, "[txco] forward %s to chassis: %v\n", sig, err)
					} else {
						fmt.Fprintf(stdout, "[txco] forwarded %s to chassis (drain)\n", sig)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// 5. Apply the bundle when --apply is set. Default is OFF so a
	// `txco dev` restart doesn't surprise the user by clobbering
	// admin-UI edits with whatever's on disk. Apply is explicit:
	// `txco dev --apply` for "I know my local OPS/ is the source of
	// truth", `txco apply` for a one-shot push later in the session.
	//
	// Non-fatal on failure: the chassis is up and serving the
	// previous active version (if any) — exiting here would kill the
	// chassis + apps and force the user to restart the whole loop
	// after a typo. Keep running and let them fix the file + `txco
	// apply` to retry.
	if *apply {
		if err := devApply(ctx, dir, resolved, ops, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "[txco] initial apply failed: %v\n", err)
			fmt.Fprintf(stderr, "[txco] chassis still running; fix the issue and run `txco apply` to retry.\n")
		}
	} else {
		fmt.Fprintf(stdout, "[txco] chassis state preserved (run `txco apply` to push local OPS/, or `txco dev --apply` to push on startup)\n")
	}

	// 5b. Optional: spawn the admin-ui Vite dev server. Non-fatal —
	// if anything's missing (admin-ui/, node_modules, package manager,
	// port) we print a clear note and continue. The chassis still
	// serves the last-built embedded bundle at /admin/.
	uiDevURL := ""
	if *uiDev {
		url, err := startUIDev(ctx, dir, stdout, &started)
		if err != nil {
			fmt.Fprintf(stderr, "[txco] --ui: %v (continuing without Vite)\n", err)
		} else {
			uiDevURL = url
		}
	}

	// 6. Watch + push-to-draft (opt-in via --watch).
	//
	// Default: no watcher. The versioned control plane treats every
	// activation as a deliberate pointer move; auto-activating on
	// every keystroke produces version churn that's misaligned with
	// the model. The user runs `txco apply` (push + activate) when
	// they're ready.
	//
	// --watch on: file changes update a per-stack sticky draft via
	// PUT /versions/{n}/files. Drafts are NOT auto-activated — the
	// chassis keeps serving the previously-active version until the
	// user explicitly runs `txco activate <stack>`.
	opsDir := filepath.Join(dir, "OPS")
	if *watch {
		// Serialize all watcher-driven re-applies (OPS draft pushes and
		// compute rebuilds) so concurrent file events can't race on
		// draft/activate against the same stack.
		var applyMu sync.Mutex

		if _, err := os.Stat(opsDir); err == nil {
			fmt.Fprintf(stdout, "[watch] watching %s (.txcl/.json → draft; colocated .js → rebuild + activate)\n", opsDir)
			state := newDevWatchState()
			// .txcl/.json → sticky draft (no auto-activation; activate to publish).
			go func() {
				err := devpkg.WatchOps(ctx, opsDir, 500*time.Millisecond, func() {
					applyMu.Lock()
					defer applyMu.Unlock()
					freshOps, err := bundle.Walk(dir)
					if err != nil {
						fmt.Fprintf(stderr, "[watch] walk: %v\n", err)
						return
					}
					if err := devApplyToDraft(ctx, dir, resolved, freshOps, state, stdout, stderr); err != nil {
						fmt.Fprintf(stderr, "[watch] push to draft: %v\n", err)
					}
				})
				if err != nil && ctx.Err() == nil {
					fmt.Fprintf(stderr, "[watch] %v\n", err)
				}
			}()

			// Colocated <name>.js → rebuild + upload + ACTIVATE. A compute
			// artifact has no admin-UI edit to clobber, so it activates live
			// (unlike the draft-only .txcl/.json watch). devApply is
			// manifest-aware, so only the stack whose digest changed re-versions.
			go func() {
				err := devpkg.WatchColocatedComputes(ctx, opsDir, 500*time.Millisecond, func() {
					applyMu.Lock()
					defer applyMu.Unlock()
					fmt.Fprintln(stdout, "[watch] compute source changed — rebuilding")
					freshOps, err := bundle.Walk(dir)
					if err != nil {
						fmt.Fprintf(stderr, "[watch] walk: %v\n", err)
						return
					}
					if err := devApply(ctx, dir, resolved, freshOps, stdout, stderr); err != nil {
						fmt.Fprintf(stderr, "[watch] compute reload: %v\n", err)
					}
				})
				if err != nil && ctx.Err() == nil {
					fmt.Fprintf(stderr, "[watch] compute: %v\n", err)
				}
			}()
		}
	}

	// 7. Wait for either a teardown signal, a child process death, or
	// (in the unlikely case both happen at once) ctx cancellation.
	if webURL != "" {
		fmt.Fprintf(stdout, "[txco] dev loop running (Ctrl-C to stop).\n")
		fmt.Fprintf(stdout, "[txco]   web inlet: %s   (curl this)\n", webURL)
		fmt.Fprintf(stdout, "[txco]   admin API: %s\n", chassisURL)
		if uiDevURL != "" {
			fmt.Fprintf(stdout, "[txco]   admin UI (HMR): %s\n", uiDevURL)
		} else {
			fmt.Fprintf(stdout, "[txco]   admin UI: %s/admin/\n", chassisURL)
		}
		// One-shot tip: hostname routing is the recommended way to
		// dispatch HTTP requests to a stack, not the legacy boot/*
		// resonators. Show this so newcomers learn the modern flow up
		// front. Always prints; doesn't probe the DB. If the user
		// has already set up hostnames, the tip is mostly a no-op
		// reminder of the verb.
		fmt.Fprintln(stdout, "[txco]   tip: bind a hostname with `txco auth tenant hostnames add localhost --stack <stack>`")
		fmt.Fprintln(stdout, "[txco]        (boot/* resonators still work; hostnames are now the recommended path)")
		if *dnsHead {
			fmt.Fprintf(stdout, "[txco]   dns head: %s (udp+tcp). create a zone with `txco dns zone create ops.example.com`,\n", devDNSListenAddr)
			fmt.Fprintf(stdout, "[txco]        then `dig @127.0.0.1 -p %s ops.example.com A`\n", strings.TrimPrefix(devDNSListenAddr, "127.0.0.1:"))
		}
	} else {
		fmt.Fprintf(stdout, "[txco] dev loop running (Ctrl-C to stop). chassis: %s\n", chassisURL)
		if uiDevURL != "" {
			fmt.Fprintf(stdout, "[txco]   admin UI (HMR): %s\n", uiDevURL)
		}
	}
	select {
	case <-abort:
		fmt.Fprintln(stdout, "[txco] signal received; tearing down")
	case <-childExit(started):
		fmt.Fprintln(stderr, "[txco] a child process exited; tearing down")
	}
	// Note: cancel() is deliberately deferred (see signal goroutine
	// above). teardown() runs via defer first, sending SIGTERM through
	// each process group before the context cancellation triggers
	// os/exec's SIGKILL on the immediate children.
	return 0
}

// devApply is the apply path used by `txco dev` — same shape as
// runApply but uses an already-resolved target and pre-walked bundle.
// devApply pushes the local bundle through the versioned control
// plane — one draft per stack, files PUT, activate. Mirrors
// runApply's push path so dev and apply share the same write model.
// The legacy flat ImportOps endpoint is retired.
func devApply(ctx context.Context, dir string, resolved ResolvedTarget, ops []bundle.Op, stdout, stderr io.Writer) error {
	out, builtComputes, cerr := resolveOpRefsColocated(ops, buildOpRefMap(resolved), dir, stderr)
	if cerr != nil {
		return cerr
	}
	for i := range out {
		if _, err := txcl.Resonator(out[i].Txcl); err != nil {
			return fmt.Errorf("parse error at %s (%s/%d/%s): %w",
				out[i].SourcePath, out[i].Stack, out[i].Scope, out[i].Name, err)
		}
	}
	if resolved.Mock == "deny" {
		for i := range out {
			out[i].MockRes = ""
		}
	}

	c := client.New(resolved.AsClientTarget())
	if err := uploadComputes(ctx, c, builtComputes, stdout, stderr); err != nil {
		return err
	}
	stacks := groupOpsByStack(out)
	totalFiles := 0
	skipped := 0
	for _, stack := range sortedKeys(stacks) {
		files := opsToFiles(stacks[stack])
		localHash := localManifestHash(files)

		// Fast paths against the chassis's current active version:
		//   1. If a saved .txco/<stack>.state.json says we last pulled
		//      v_saved and the chassis's active is now > v_saved, the
		//      admin UI moved ahead of us. Pushing would clobber those
		//      edits, so warn loudly and skip this stack.
		//   2. If the chassis's active manifest already matches local,
		//      there's nothing to do — skip the version churn.
		// Anything else falls through to the existing create/push/
		// activate path.
		st, getErr := c.GetStack(ctx, stack)
		if getErr == nil && st != nil && st.ActiveVersion != nil {
			active := *st.ActiveVersion
			saved, _ := state.Load(dir, stack)
			if saved != nil && active > saved.VersionNumber {
				fmt.Fprintf(stderr,
					"[txco] %s: chassis active v%d is ahead of locally-pulled v%d — admin UI edits not pulled.\n",
					stack, active, saved.VersionNumber)
				fmt.Fprintf(stderr,
					"[txco]   keeping chassis state; run `txco pull %s --force` to overwrite local OPS/, or `txco apply` to overwrite chassis.\n",
					stack)
				skipped++
				continue
			}
			vd, vErr := c.GetVersion(ctx, stack, active, false)
			if vErr == nil && vd != nil && vd.ManifestHash != "" && vd.ManifestHash == localHash {
				fmt.Fprintf(stdout,
					"[txco] %s v%d already matches local (manifest %s…) — skipped\n",
					stack, active, localHash[:8])
				// Refresh the local state pointer so subsequent restarts
				// see the active number as the parent.
				_ = state.Save(dir, stack, state.State{
					VersionNumber:       active,
					ParentVersionNumber: active,
					ManifestHash:        vd.ManifestHash,
				})
				totalFiles += len(files)
				skipped++
				continue
			}
		}

		// Push path. "active" tells the server to clone from the
		// current active version when one exists, otherwise start an
		// empty draft. Mirrors runApply (apply.go:134).
		versionNumber, err := c.CreateDraft(ctx, stack, "active")
		if err != nil {
			return fmt.Errorf("%s: create draft: %w", stack, err)
		}
		if _, err := c.PutDraftFiles(ctx, stack, versionNumber, files); err != nil {
			return fmt.Errorf("%s: put files for v%d: %w", stack, versionNumber, err)
		}
		// Pre-activate validation: surface parse / ref / graph errors
		// before the pointer flips. The chassis already runs these on
		// activate; calling validate first lets the dev loop print
		// per-file diagnostics and bail without leaving an active
		// version in a broken state. Failure leaves the draft on the
		// chassis so the user can either fix locally and restart or
		// activate manually via the admin UI after investigating.
		if vresp, verr := c.ValidateVersion(ctx, stack, versionNumber); verr == nil && vresp != nil && !vresp.OK {
			fmt.Fprintf(stderr,
				"[txco] %s v%d: validation failed (%d error%s); not activating.\n",
				stack, versionNumber, len(vresp.Errors), pluralS(len(vresp.Errors)))
			for _, e := range vresp.Errors {
				fmt.Fprintf(stderr, "[txco]   %s: %s\n", e.Path, e.Err)
			}
			fmt.Fprintf(stderr,
				"[txco] draft v%d left on chassis; fix locally and restart, or activate via admin UI after investigating.\n",
				versionNumber)
			continue
		}
		if _, err := c.Activate(ctx, stack, versionNumber); err != nil {
			return fmt.Errorf("%s: activate v%d: %w", stack, versionNumber, err)
		}
		// Persist the new pointer so a future restart will skip when
		// nothing changed, and so the divergence check has a baseline.
		if err := state.Save(dir, stack, state.State{
			VersionNumber:       versionNumber,
			ParentVersionNumber: versionNumber,
			ManifestHash:        localHash,
		}); err != nil {
			fmt.Fprintf(stderr, "[txco] %s: warn: save state: %v\n", stack, err)
		}
		fmt.Fprintf(stdout, "[txco] %s v%d activated (%d files)\n", stack, versionNumber, len(files))
		totalFiles += len(files)
	}
	if skipped > 0 {
		fmt.Fprintf(stdout, "[txco] applied %d stack(s), %d file(s), %d skipped (no changes / diverged)\n", len(stacks), totalFiles, skipped)
	} else {
		fmt.Fprintf(stdout, "[txco] applied %d stack(s), %d file(s)\n", len(stacks), totalFiles)
	}
	return nil
}

// devWatchState holds the per-stack draft version_number that the
// watcher reuses across file events. Bounded in memory; nothing is
// persisted — `txco dev` exit forgets every draft and the next run
// creates fresh ones. The mutex guards concurrent watcher fires
// (the bundle walk + push isn't atomic from the watch loop's
// perspective if a second save lands while the first is in flight).
type devWatchState struct {
	mu     sync.Mutex
	drafts map[string]int64 // stack name → current draft version_number
}

func newDevWatchState() *devWatchState {
	return &devWatchState{drafts: map[string]int64{}}
}

// devApplyToDraft is the watcher's push path. It maintains one
// sticky draft per stack and PUTs the latest file set to it on every
// fire — no activation. If the tracked draft has been activated out
// from under us (status flipped to 'superseded'), the server returns
// 409 version_not_draft and we transparently create a fresh draft
// and retry once.
func devApplyToDraft(ctx context.Context, dir string, resolved ResolvedTarget, ops []bundle.Op, state *devWatchState, stdout, stderr io.Writer) error {
	out, builtComputes, cerr := resolveOpRefsColocated(ops, buildOpRefMap(resolved), dir, stderr)
	if cerr != nil {
		return cerr
	}
	for i := range out {
		if _, err := txcl.Resonator(out[i].Txcl); err != nil {
			return fmt.Errorf("parse error at %s (%s/%d/%s): %w",
				out[i].SourcePath, out[i].Stack, out[i].Scope, out[i].Name, err)
		}
	}
	if resolved.Mock == "deny" {
		for i := range out {
			out[i].MockRes = ""
		}
	}

	c := client.New(resolved.AsClientTarget())
	// Upload any colocated computes so the draft can be activated later (the
	// activate-time presence check needs the artifact present).
	if err := uploadComputes(ctx, c, builtComputes, stdout, stderr); err != nil {
		return err
	}
	stacks := groupOpsByStack(out)
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, stack := range sortedKeys(stacks) {
		files := opsToFiles(stacks[stack])
		if n, ok := state.drafts[stack]; ok {
			if _, err := c.PutDraftFiles(ctx, stack, n, files); err == nil {
				fmt.Fprintf(stdout, "[watch] %s v%d updated (%d files)\n", stack, n, len(files))
				continue
			} else if !isVersionNotDraftErr(err) {
				return fmt.Errorf("%s: put files: %w", stack, err)
			}
			// Tracked draft was activated externally — fall through
			// to create a fresh one.
			delete(state.drafts, stack)
		}
		n, err := c.CreateDraft(ctx, stack, "active")
		if err != nil {
			return fmt.Errorf("%s: create draft: %w", stack, err)
		}
		if _, err := c.PutDraftFiles(ctx, stack, n, files); err != nil {
			return fmt.Errorf("%s: put files for v%d: %w", stack, n, err)
		}
		state.drafts[stack] = n
		fmt.Fprintf(stdout, "[watch] %s new draft v%d (%d files) — run `txco activate %s` to publish\n",
			stack, n, len(files), stack)
	}
	return nil
}

// isVersionNotDraftErr reports whether `err` is the 409 the chassis
// returns when PUTting files to a non-draft version (e.g. it was
// activated externally while we were tracking it).
func isVersionNotDraftErr(err error) bool {
	var he *client.HTTPError
	if errors.As(err, &he) {
		return he.StatusCode == http.StatusConflict && he.Code == "version_not_draft"
	}
	return false
}

// startChassis spawns a chassis subprocess pointed at a per-workspace
// temp DB. Returns the admin URL and the web URL (the curlable one
// developers actually hit during dev).
//
// tcpHead controls whether the TCP personality is included. Off by
// default — most dev workflows use only web + cron + admin, and the
// :5050 bind otherwise causes spurious "port in use" failures on
// machines running other things there.
func startChassis(ctx context.Context, workspace, addrOverride, webAddrOverride string, tcpHead, dnsHead, verbose bool, stdout, stderr io.Writer, started *[]*devpkg.Process, out **devpkg.Process) (adminURL, webURL string, err error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("locate self: %w", err)
	}

	devDir := filepath.Join(workspace, ".txco", "dev")
	dbDir := filepath.Join(devDir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", dbDir, err)
	}

	// txco dev uses the canonical chassis ports so the URLs are
	// predictable from the docs without reading the boot log. Each
	// port is fail-fast: if something's already bound, surface a clear
	// error rather than silently picking a random port (which leaves
	// users curling the wrong address).
	adminAddr := addrOverride
	if adminAddr == "" {
		adminAddr = ":8081"
	}
	if err := requirePortFree(adminAddr, "admin API"); err != nil {
		return "", "", err
	}
	webAddr := webAddrOverride
	if webAddr == "" {
		webAddr = ":8080"
	}
	if err := requirePortFree(webAddr, "web inlet"); err != nil {
		return "", "", err
	}
	tcpAddr := ":5050"
	if tcpHead {
		if err := requirePortFree(tcpAddr, "TCP inlet"); err != nil {
			return "", "", err
		}
	}
	dnsAddr := devDNSListenAddr
	if dnsHead {
		if err := requirePortFree(dnsAddr, "DNS inlet"); err != nil {
			return "", "", err
		}
	}

	// Locate the schema dir relative to the workspace; fall back to a
	// known repo-relative path.
	schemaDir := findSchemaDir(workspace)

	// The chassis defaults a handful of data dirs (kv store, logs,
	// admin static, docker tmp, repo/continuation store, artifact store,
	// feed source, secret master key) to `./chassis/data/*` —
	// relative to whatever CWD it boots from. When `txco dev` runs the
	// chassis with the user's workspace as CWD, those defaults would
	// drop `chassis/data/...` directories into the workspace. Redirect
	// every workspace-relative chassis path under `.txco/dev/` so the
	// workspace itself stays clean (just the user's OPS/, APPS/,
	// txco.yaml). The whole `.txco/` tree is gitignored.
	kvDir := filepath.Join(devDir, "kv")
	logsDir := filepath.Join(devDir, "logs")
	adminStaticDir := filepath.Join(devDir, "admin", "static")
	dockerTmpDir := filepath.Join(devDir, "tmp", "docker")
	repoDir := filepath.Join(devDir, "repo")
	continuationsDir := filepath.Join(devDir, "continuations")
	artifactsDir := filepath.Join(devDir, "artifacts")
	feedDir := filepath.Join(devDir, "feed")
	secretKeyPath := filepath.Join(devDir, "secrets", "txco-master.key")

	// Hard-coded path overrides: workspace-scoped state has to land
	// under .txco/dev/, otherwise chassis defaults would litter the
	// repo with chassis/data/*. These are always set, never overridden
	// by the parent env.
	env := []string{
		"TXCO_ADMIN_ADDR=" + adminAddr,
		"TXCO_WEB_ADDR=" + webAddr,
		"TXCO_DB_ROOT_DIR=" + dbDir,
		"TXCO_KVSTORE_ADDRS=" + kvDir,
		"TXCO_LOG_OPS_DIR=" + logsDir,
		"TXCO_ADMIN_ROOT_DIR=" + adminStaticDir,
		"TXCO_DOCKER_TMP=" + dockerTmpDir,
		"TXCO_REPOSTORE_FILE_DIR=" + repoDir,
		"TXCO_CONTINUATION_STORE_FILE_DIR=" + continuationsDir,
		"TXCO_ARTIFACT_STORE_FILE_DIR=" + artifactsDir,
		"TXCO_FEED_SOURCE_FILE_DIR=" + feedDir,
		"TXCO_SECRET_MASTER_KEY=" + secretKeyPath,
	}

	// Personality set. We pin it to cron+web+admin and add the heavier
	// heads (tcp, dns) only when opted in via --tcp / --dns. Each head's
	// *ListenAddrs env is set only when that head is on (otherwise the
	// chassis would bind the head's default port per its own default);
	// pinning Personalities suppresses an un-opted head entirely (see the
	// strings.Contains gate in each personality's Start).
	heads := []string{"cron", "web", "admin"}
	if tcpHead {
		heads = append(heads, "tcp")
		env = append(env, "TXCO_TCP_LISTEN_ADDRS="+tcpAddr)
	}
	if dnsHead {
		heads = append(heads, "dns")
		env = append(env, "TXCO_DNS_LISTEN_ADDRS="+dnsAddr)
	}
	env = append(env, "TXCO_PERSONALITIES="+strings.Join(heads, ","))

	// Dev-default toggles: enabled by default so devs get full
	// breakpoints/private/trace out of the box. Set-if-missing: a
	// parent env that already has any of these (e.g. for a `hey`
	// benchmark with logging disabled) takes precedence:
	//
	//   TXCO_TRACE_MODE=off TXCO_LOG_LEVEL=error txco dev
	//
	// turns trace off and quiets logs without editing source.
	devDefaults := map[string]string{
		// INFO by default so `txco dev` isn't drowned in chassis-internal
		// DEBUG noise. `--verbose` flips to DEBUG; a parent
		// TXCO_LOG_LEVEL=... still wins (set-if-missing).
		"TXCO_LOG_LEVEL":         "info",
		"TXCO_DEBUG_BREAKPOINTS": "true",
		"TXCO_DEBUG_PRIVATE":     "true",
		// Unauthenticated admin on loopback — the documented dev posture.
		// `basic` mode IGNORES request signatures and, with no basic
		// creds set, treats every caller as open-dev (admin:all). Without
		// this, dev runs in the chassis default `both` mode, which
		// VERIFIES signatures — so a developer whose machine has an
		// ambient signing profile gets 401 unknown_key from a fresh dev
		// chassis that's never seen their key. Set-if-missing, so
		// `TXCO_AUTH_MODE=both txco dev` opts back into signed-auth testing.
		"TXCO_AUTH_MODE":   "basic",
		"TXCO_TRACE_MODE":  "full",
		"TXCO_TRACE_DIR":   filepath.Join(devDir, "trace"),
		"TXCO_TRACE_ASYNC": "true",
		// Localhost hostnames resolve to 127.0.0.1 and other private
		// addresses; the SSRF blocklist would reject them in
		// production. Allow the verifier through in dev.
		"TXCO_VERIFY_ALLOW_PRIVATE_ADDRESSES": "true",
		// Zero-flag structured hostnames in dev: every activated stack
		// is reachable at http://<stack>-<rand>.localhost:<webport>
		// (*.localhost is loopback + a browser secure context over
		// HTTP — no certs). Set-if-missing, so
		// `TXCO_STRUCTURED_HOST_SUFFIX= txco dev` disables it and a
		// custom suffix overrides. The library/`txco serve` default
		// stays "" (embedder behavior unchanged).
		"TXCO_STRUCTURED_HOST_SUFFIX": ".localhost",
		// System stacks (OPS/_sys/boot, …) live in the same OPS/ tree
		// as application stacks, discriminated by the `_` prefix.
		// Point the loader at the workspace root and hot-reload in dev
		// only. `txco serve` leaves watch off (static after boot).
		"TXCO_SYSTEM_OPSTACKS_DIR":   workspace,
		"TXCO_SYSTEM_OPSTACKS_WATCH": "true",
	}
	// DNS synthesis infra defaults (only when the head is on). Edge =
	// loopback so synthesized A records point where the dev chassis
	// actually serves; MX = localhost (the LMTP head); placeholder NS
	// names. Set-if-missing like the rest, so
	// `TXCO_DNS_EDGE_IPS=… txco dev --dns` overrides. The chassis serve
	// default for these stays empty (operator must configure in prod).
	if dnsHead {
		devDefaults["TXCO_DNS_NAMESERVERS"] = "ns1.localhost,ns2.localhost"
		devDefaults["TXCO_DNS_EDGE_IPS"] = "127.0.0.1"
		devDefaults["TXCO_DNS_MX_HOST"] = "localhost"
	}
	for k, v := range devDefaults {
		if _, set := os.LookupEnv(k); !set {
			env = append(env, k+"="+v)
		}
	}

	// --verbose wins over both default and parent env. Appended last
	// so it overrides any prior TXCO_LOG_LEVEL in the env slice.
	if verbose {
		env = append(env, "TXCO_LOG_LEVEL=debug")
	}

	// Point the spawned chassis at a dev-scoped master-key path so
	// the file lands under .txco/dev/ (gitignored) instead of the
	// default ./chassis/data/secrets/. The chassis's boot path
	// auto-mints on first run via secrets.LoadOrMintFileMasterKey —
	// same UX as the runtime DB. Honors a parent
	// TXCO_SECRET_MASTER_KEY override.
	if _, set := os.LookupEnv("TXCO_SECRET_MASTER_KEY"); !set {
		keyPath := filepath.Join(devDir, "secrets", "txco-dev-master.key")
		env = append(env, "TXCO_SECRET_MASTER_KEY="+keyPath)
	}
	if schemaDir != "" {
		env = append(env, "TXCO_DB_SCHEMA_DIR="+schemaDir)
	}
	// Disable basic auth in dev (chassis emits a WARN at boot).

	tcpDesc := "off"
	if tcpHead {
		tcpDesc = tcpAddr
	}
	fmt.Fprintf(stdout, "[txco] starting chassis (admin=%s, web=%s, tcp=%s, db=%s)\n", adminAddr, webAddr, tcpDesc, dbDir)
	p, err := devpkg.Spawn(ctx, devpkg.SpawnConfig{
		Name: "chassis",
		Cmd:  shellEscape(executable) + " serve",
		Out:  stdout,
		Env:  env,
	})
	if err != nil {
		return "", "", fmt.Errorf("spawn chassis: %w", err)
	}
	*started = append(*started, p)
	*out = p

	adminURL = "http://localhost" + adminAddr
	webURL = "http://localhost" + webAddr
	if err := devpkg.WaitHealthy(ctx, adminURL+"/healthz", 30*time.Second, 500*time.Millisecond); err != nil {
		return "", "", fmt.Errorf("chassis health: %w", err)
	}
	return adminURL, webURL, nil
}

// startUIDev locates admin-ui/ by walking up from workspace, picks
// a package manager (prefer pnpm — that's what package.json declares —
// fall back to npm), and spawns `<pm> run dev`. Returns the URL the
// developer should open. The spawned process joins `started` so the
// dev loop's normal teardown handles it.
//
// Returning a typed error rather than logging directly so the caller
// can decide whether to fail or merely warn.
func startUIDev(ctx context.Context, workspace string, stdout io.Writer, started *[]*devpkg.Process) (string, error) {
	uiDir, ok := findAdminUI(workspace)
	if !ok {
		return "", fmt.Errorf("admin-ui/ not found above %q (cloned thanks-computer monorepo only)", workspace)
	}
	if _, err := os.Stat(filepath.Join(uiDir, "node_modules")); os.IsNotExist(err) {
		return "", fmt.Errorf("admin-ui dependencies missing — run `cd %s && pnpm install` first", uiDir)
	}
	pm, err := pickPackageManager(uiDir)
	if err != nil {
		return "", err
	}
	if err := requirePortFree(adminUIDevPort, "admin-ui dev"); err != nil {
		return "", err
	}

	fmt.Fprintf(stdout, "[txco] starting admin-ui dev server (%s run dev) in %s\n", pm, uiDir)
	p, err := devpkg.Spawn(ctx, devpkg.SpawnConfig{
		Name: "admin-ui",
		Dir:  uiDir,
		Cmd:  pm + " run dev",
		Out:  stdout,
		// Vite respects FORCE_COLOR; keep its output readable when
		// piped through the tagged writer.
		Env: []string{"FORCE_COLOR=1"},
	})
	if err != nil {
		return "", err
	}
	*started = append(*started, p)

	// Wait for Vite to bind. Best-effort: a few short polls so the
	// startup banner can include a URL the user can click. We don't
	// fail dev if Vite takes longer than expected — the process is
	// still managed and its output will appear with the [admin-ui] tag.
	url := "http://localhost" + adminUIDevPort
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !portFree(adminUIDevPort) {
			return url + "/", nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return url + "/", nil
}

// findAdminUI walks up from start looking for `admin-ui/package.json`.
// Returns the admin-ui directory and true on success. Walks at most
// 8 levels — deep enough for any sane checkout, bounded so a stat-loop
// can't run away.
func findAdminUI(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "admin-ui")
		if info, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil && !info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

// pickPackageManager prefers pnpm (matches package.json's
// packageManager field), falling back to npm. Returns the bare
// executable name, suitable for `<name> run dev`.
func pickPackageManager(uiDir string) (string, error) {
	hint := strings.ToLower(packageManagerHint(uiDir))
	if strings.HasPrefix(hint, "pnpm") || hint == "" {
		if _, err := exec.LookPath("pnpm"); err == nil {
			return "pnpm", nil
		}
	}
	if _, err := exec.LookPath("npm"); err == nil {
		return "npm", nil
	}
	if _, err := exec.LookPath("pnpm"); err == nil {
		return "pnpm", nil
	}
	return "", fmt.Errorf("no package manager on PATH (need pnpm or npm)")
}

// packageManagerHint reads package.json's "packageManager" field
// (e.g. "pnpm@10.6.3"). Returns "" when absent or unreadable —
// callers fall back to PATH-detection.
func packageManagerHint(uiDir string) string {
	raw, err := os.ReadFile(filepath.Join(uiDir, "package.json"))
	if err != nil {
		return ""
	}
	// Tiny regex-free parse: scan for the literal "packageManager".
	// Avoids pulling encoding/json into a path that runs on every
	// `txco dev --ui` for a 30-byte lookup.
	const key = `"packageManager"`
	i := strings.Index(string(raw), key)
	if i < 0 {
		return ""
	}
	tail := string(raw[i+len(key):])
	colon := strings.Index(tail, ":")
	if colon < 0 {
		return ""
	}
	tail = tail[colon+1:]
	q1 := strings.Index(tail, `"`)
	if q1 < 0 {
		return ""
	}
	tail = tail[q1+1:]
	q2 := strings.Index(tail, `"`)
	if q2 < 0 {
		return ""
	}
	return tail[:q2]
}

// portFree reports whether anything is currently bound to addr.
// addr can be ":8081" or "127.0.0.1:8081" — both shapes are tried via
// net.Listen on tcp.
func portFree(addr string) bool {
	if !strings.Contains(addr, ":") {
		return false
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// requirePortFree returns nil if the port is currently free, or a clear
// error otherwise. The error tells the user exactly which port collided
// and how to resolve it.
func requirePortFree(addr, role string) error {
	if portFree(addr) {
		return nil
	}
	return fmt.Errorf(
		"port %s (%s) is already in use; stop whatever's bound to it "+
			"(try: lsof -i %s) or pass --no-chassis to skip spawning a chassis "+
			"and reuse the one already running",
		addr, role, addr)
}

func findSchemaDir(workspace string) string {
	// Look for a sibling repo checkout — common dev layout is
	// `<workspace>` next to `<workspace>/db/schema/sqlite/` or in a sibling
	// `thanks-computer/` checkout. Try a few sensible locations.
	candidates := []string{
		filepath.Join(workspace, "db", "schema", "sqlite"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// shellEscape quotes a single argument for `sh -c`. Lightweight: the
// only special character we worry about for an executable path is
// whitespace and quotes.
func shellEscape(s string) string {
	if !strings.ContainsAny(s, " \t'\"\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func orderedAppNames(apps map[string]appConfig) []string {
	names := make([]string, 0, len(apps))
	for n := range apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func targetNames(targets map[string]targetConfig) []string {
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ensureGitignored makes sure the workspace's .gitignore lists `entry`.
// Returns (added=true) when the file was created or the line appended,
// (added=false, nil) when the entry was already present. Idempotent and
// best-effort: any I/O error returns (false, err) and the caller is
// expected to log-and-continue rather than fail the dev loop.
func ensureGitignored(workspace, entry string) (bool, error) {
	path := filepath.Join(workspace, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	// Look for the entry on any line (allowing leading/trailing space).
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == entry {
			return false, nil
		}
	}
	// Append, ensuring the file ends with a newline before our addition.
	var out []byte
	if len(existing) == 0 {
		out = []byte(entry + "\n")
	} else {
		if !strings.HasSuffix(string(existing), "\n") {
			existing = append(existing, '\n')
		}
		out = append(existing, []byte(entry+"\n")...)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// childExit returns a channel that closes when any of the spawned
// processes exit unexpectedly. Multiple goroutines may race to close;
// sync.Once guards against double-close panics.
func childExit(procs []*devpkg.Process) <-chan struct{} {
	out := make(chan struct{})
	if len(procs) == 0 {
		return out // never fires
	}
	var once sync.Once
	closer := func() { once.Do(func() { close(out) }) }
	for _, p := range procs {
		ch := p.Done()
		go func() {
			<-ch
			closer()
		}()
	}
	return out
}
