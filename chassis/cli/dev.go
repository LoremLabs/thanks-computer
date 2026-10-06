package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	devpkg "github.com/loremlabs/thanks-computer/chassis/cli/dev"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/sysops"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

const adminUIDevPort = ":6161"

// devDNSListenAddr is the loopback address `txco dev --dns` binds the
// authoritative-DNS head on (UDP+TCP). Deliberately NOT :5353 — that's
// mDNS on macOS and would clash. dig with `-p 5354`.
const devDNSListenAddr = "127.0.0.1:5354"

// devLMTPListenAddr is the address `txco dev --lmtp` binds the mail head
// on — the same :2424 the docs, examples, and swaks invocations use.
const devLMTPListenAddr = ":2424"

// devIMAPListenAddr / devIMAPTLSAddr are the loopback addresses `txco dev
// --imap` binds the IMAP head on: plaintext (+ STARTTLS) and IMAPS. Both
// serve a self-signed certificate minted at boot — desktop mail clients
// refuse LOGIN over plaintext, so the account is added with SSL on port
// 1993 and the certificate trusted once.
const (
	devIMAPListenAddr = "127.0.0.1:1143"
	devIMAPTLSAddr    = "127.0.0.1:1993"
	// devIPPTLSAddr is the HTTPS listener `txco dev --ipp` adds to the web
	// head: print clients speak IPPS, and the plain web port cannot.
	devIPPTLSAddr = "127.0.0.1:8443"
)

// devMailRelayAddr is where `txco dev --lmtp` relays OUTBOUND mail by
// default: the conventional local sink (MailHog/Mailpit SMTP port,
// plaintext). A dev chassis should never be able to deliver real mail
// by accident; overriding TXCO_MAIL_RELAY_ADDR is the deliberate act.
const devMailRelayAddr = "localhost:1025"

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
	tcpHead := fs.Bool("tcp", false, "start the TCP head (binds :5050; override with TXCO_TCP_LISTEN_ADDRS, e.g. 'irc=127.0.0.1:6697;self-signed' for a TLS/SNI listener). Disabled by default — most workflows only need web + cron + admin.")
	dnsHead := fs.Bool("dns", false, "start the authoritative-DNS head with dev defaults: binds "+devDNSListenAddr+" (UDP+TCP) and pre-sets synthesis infra (nameservers ns1/ns2.localhost, edge 127.0.0.1, MX localhost) so a delegated zone resolves out of the box. Disabled by default. Override any of TXCO_DNS_NAMESERVERS/EDGE_IPS/MX_HOST.")
	stateHead := fs.Bool("state", false, "start the state personality (the dispatcher behind txco://state/*; presents each committed transition into the tenant's _state stack). Disabled by default; the dev store lands at .txco/dev/state.db.")
	grantHead := fs.Bool("grant", false, "start the grant personality: the Unix socket `txco sandbox` reaches the chassis on, so a program a command runs can open a sandbox itself. Disabled by default; without it a command holds only what `WITH sandbox` opened for it. Needs --allow-local-workspace to hand a grant to a command.")
	scheduledHead := fs.Bool("scheduled", false, "start the scheduled personality (the durable-timer poller behind txco://schedule; fires due events into each tenant's _scheduled stack). Disabled by default; the dev store lands at .txco/dev/scheduled.db.")
	allowLocalWorkspace := fs.Bool("allow-local-workspace", false, "enable workspace:// ops backed by the local provider: commands run as YOUR uid, unsandboxed, under .txco/dev/workspaces/<tenant>/<stack>/<name>. Off by default and never implied by anything else; the chassis logs a WARN pair when it is on.")
	workspaceLocalExec := fs.String("workspace-local-exec", "", "with --allow-local-workspace: hand every workspace command to this prefix program instead of running it here, so the local provider drives another machine, e.g. \"sprite exec -s dev-{name} --\" or \"docker exec -i pony-{name}\". Whitespace-separated words, no quoting; {tenant}, {stack} and {name} are replaced. The command's variables (a run grant's token among them) travel inside the command; the machine itself is yours to make and remove.")
	sourceHead := fs.Bool("source", false, "start the source personality (the remote-mailbox poller: dials OUT to each SOURCES/-declared IMAP mailbox, reads past a durable cursor, and fires every new message into the source's _source stack). Disabled by default. Needs a mailbox password in the tenant secret store; egress is open in dev, so a source may point at loopback (e.g. this chassis's own --imap head).")
	imapHead := fs.Bool("imap", false, "start the IMAP head with dev defaults: binds "+devIMAPListenAddr+" (plaintext + STARTTLS, LOGIN allowed over loopback) and "+devIMAPTLSAddr+" (IMAPS) with a self-signed certificate minted at boot, index at .txco/dev/imap.db — so a stack can provision an account with txco://imap/account, issue its password with txco://credential/create, and a mail client can open it (server <dev host>, port 1993, SSL on, trust the certificate). Disabled by default. Override TXCO_IMAP_LISTEN_ADDRS/IMAP_TLS_ADDRS/IMAP_DB_PATH.")
	calendarHead := fs.Bool("calendar", false, "start the calendar personality (CalDAV + ICS feeds) on the web head with dev defaults: served under http://<dev host>:<web port>/dav/ with Basic auth allowed over plaintext, index at .txco/dev/calendar.db — so a stack can provision an account with txco://calendar/account, issue its password with txco://credential/create, and a calendar app can open it (server <bound-host>, port = the web port, SSL off, path /dav/). Disabled by default. Override TXCO_CALENDAR_DB_PATH/CALENDAR_PATH_PREFIX.")
	contactsHead := fs.Bool("contacts", false, "start the contacts personality (CardDAV) on the web head with dev defaults: served under http://<dev host>:<web port>/carddav/ with Basic auth allowed over plaintext, index at .txco/dev/contacts.db — so a stack can provision an account with txco://contacts/account, issue its password with txco://credential/create, and a contacts app can open it (server <bound-host>, port = the web port, SSL off, path /carddav/). Disabled by default. Override TXCO_CONTACTS_DB_PATH/CONTACTS_PATH_PREFIX.")
	webdavHead := fs.Bool("webdav", false, "start the webdav personality (the drive store as a mountable folder) on the web head with dev defaults: served under http://<dev host>:<web port>/drive/ with Basic auth allowed over plaintext, index at .txco/dev/drive.db and the bytes under .txco/dev/drive/ — so a stack can provision an account with txco://drive/collection + txco://drive/account, issue its password with txco://credential/create, and Finder / rclone can mount it (server <bound-host>, port = the web port, SSL off, path /drive/). Disabled by default. Override TXCO_DRIVE_DB_PATH/DRIVE_OBJECTS_FILE_DIR/DRIVE_PATH_PREFIX.")
	ippHead := fs.Bool("ipp", false, "start the ipp personality (a stack presented as a PRINTER) with dev defaults: the web head also listens on "+devIPPTLSAddr+" (HTTPS, self-signed certificate kept at .txco/dev/web-selfsigned.crt) and answers IPP on hosts named ipp.<bound-host> — ipps://ipp.localhost:8443/p/<printer>; job store at .txco/dev/ipp.db; every request's operation and attribute NAMES are logged. A tenant has printers only once it has an active `_ipp` stack AND a registered printer (txco://ipp/printer); a client signs in as the printer's principal, with a password from txco://credential/create. Disabled by default. Override TXCO_WEB_TLS_ADDR/IPP_DB_PATH/IPP_WIRE_DEBUG.")
	lmtpHead := fs.Bool("lmtp", false, "start the LMTP mail head with dev defaults: binds "+devLMTPListenAddr+", relays outbound mail to a local sink ("+devMailRelayAddr+", TLS off — MailHog/Mailpit), and auto-loads ./ingress.yaml from the workspace when present (the local stand-in for minted hostnames). Disabled by default. Override any of TXCO_LMTP_LISTEN_ADDRS/MAIL_RELAY_ADDR/MAIL_RELAY_TLS/INGRESS_CONFIG.")
	staticOnly := fs.Bool("static-only", false, "deploy a bound Web ABI build's static half even though it has a server entry (no chassis runs server/ yet)")
	watch := fs.Bool("watch", true, "watch sources and hot-reload: compute edits rebuild + reactivate; OPS edits push to a per-stack draft. On by default (that's what `dev` is for); pass --watch=false to disable.")
	watchIgnore := fs.StringArray("watch-ignore", nil, "glob pattern (repeatable) whose matching directories are pruned from the watcher — CPU saver in big workspaces. A pattern with `/` matches a dir's path under OPS/ (e.g. `publications/*/FILES`); a bare name matches anywhere (e.g. `node_modules`). Per-stack FILES/ trees are pruned by default; add `dev.watch.includeFiles: true` to txco.yaml to watch them.")
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
		if err == pflag.ErrHelp {
			return 0
		}
		// pflag only prints the error itself under ExitOnError; with
		// ContinueOnError a typo'd flag would otherwise exit 2 in silence.
		fmt.Fprintf(stderr, "dev: %v\n(chassis flags such as --imap-wire-debug are environment variables here: TXCO_IMAP_WIRE_DEBUG=true txco dev …; `txco dev --help` lists the dev flags)\n", err)
		return 2
	}

	// A prefix without the provider would be a setting nothing reads; and the
	// provider is never implied by anything but its own flag.
	if strings.TrimSpace(*workspaceLocalExec) != "" && !*allowLocalWorkspace {
		fmt.Fprintln(stderr, "dev: --workspace-local-exec needs --allow-local-workspace (it is the local provider's own setting)")
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
	ws, err := readWorkspace(dir)
	if err != nil {
		fmt.Fprintf(stderr, "dev: %v\n", err)
		return 1
	}
	ops := ws.Ops
	for _, n := range sortedMapKeys(ws.Broken) {
		fmt.Fprintf(stderr, "[txco] %s: %v — it deploys once the build exists\n", n, ws.Broken[n])
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
	var devProfileAction auth.DevProfileAction
	if !*noChassis {
		chassisURL, webURL, err = startChassis(ctx, chassisOpts{
			Workspace: dir, AdminAddr: *chassisAddr, WebAddr: *webAddr,
			Heads: devHeads{
				TCP: *tcpHead, DNS: *dnsHead, LMTP: *lmtpHead, Scheduled: *scheduledHead, Source: *sourceHead,
				IMAP: *imapHead, Calendar: *calendarHead, Contacts: *contactsHead, WebDAV: *webdavHead,
				IPP: *ippHead, State: *stateHead, Grant: *grantHead,
			},
			AllowLocalWorkspace: *allowLocalWorkspace, LocalExec: *workspaceLocalExec, Verbose: *verbose,
			Stdout: stdout, Stderr: stderr, Started: &started, Out: &chassisProc,
		})
		if err != nil {
			fmt.Fprintf(stderr, "dev: %v\n", err)
			return 1
		}
		// Override the resolved target's chassis URL so apply/diff
		// inside this process talks to the spawned chassis.
		resolved.Chassis = chassisURL

		// Register a keyless `dev` profile pointing at this chassis so the
		// developer gets a named target — `txco apply dev`, `txco ui dev`,
		// `txco auth tenant secrets set X dev` — without `bootstrap-local`.
		// Not made active, so prod/cloud commands are untouched. Do it now (so
		// the profile exists before the initial apply), but defer the
		// user-facing line to the banner below. Non-fatal: the loop still works
		// via `--target <url>` if this can't be written.
		var perr error
		if devProfileAction, _, perr = auth.EnsureDevProfile(auth.DevProfileName, chassisURL, auth.DefaultTenantSlug); perr != nil {
			fmt.Fprintf(stderr, "[txco] note: couldn't register %q profile: %v\n", auth.DevProfileName, perr)
		}
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
		if err := devApply(ctx, ws, resolved, *staticOnly, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "[txco] initial apply failed: %v\n", err)
			fmt.Fprintf(stderr, "[txco] chassis still running; fix the issue and run `txco apply` to retry.\n")
		}
	} else {
		fmt.Fprintf(stdout, "[txco] chassis state preserved (run `txco apply` to push local OPS/, or `txco dev --apply` to push on startup)\n")
	}

	// Resolve + record each stack's reachable dev URL (structured hostname +
	// web port) so they're visible here and to a separate `txco status`.
	recordDevURLs(ctx, resolved, dir, webURL, stdout)

	// Make a single-stack workspace answer on plain http://localhost:<port>.
	// The minted structured hostname (<stack>-<rand>.localhost) only
	// resolves in browsers — CLI clients (curl scripts, Claude Code via
	// ANTHROPIC_BASE_URL) use the system resolver, which doesn't answer
	// *.localhost subdomains. With exactly one non-underscore stack the
	// bind is unambiguous, so do it; multi-stack workspaces keep the
	// manual tip below. Gated on the startup apply so we never bind
	// localhost to a stack the chassis doesn't have.
	localhostStack := ""
	if *apply {
		localhostStack = devAutoBindLocalhost(ctx, resolved, dir, ws.Ops, webURL, stdout)
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
			// Watcher tuning: prune big/irrelevant subtrees so the polling
			// watcher doesn't lstat tens of thousands of built assets 10×/sec.
			// FILES/ asset trees are excluded by default (see Options.IncludeFiles);
			// txco.yaml `dev.watch.ignore` + --watch-ignore add more.
			// Files the ops pull in with &include: editing one re-applies like
			// editing the .txcl. So does any file of a stack whose ops name
			// $TXCO_STACK_DIR: its whole directory ships as the stack tree.
			// Re-read from every walk, so a newly added include or tree is
			// watched from the next save on.
			type watched struct {
				files map[string]bool
				trees []string // stack directories, with a trailing separator
			}
			var included atomic.Pointer[watched]
			noteIncludes := func(ops []bundle.Op) {
				w := watched{files: map[string]bool{}}
				for _, op := range ops {
					for _, p := range op.Includes {
						w.files[filepath.Join(dir, filepath.FromSlash(p))] = true
					}
				}
				for stack, sops := range groupOpsByStack(ops) {
					if usesStackDir(sops) {
						w.trees = append(w.trees, filepath.Join(dir, "OPS", filepath.FromSlash(stack))+string(filepath.Separator))
					}
				}
				included.Store(&w)
			}
			noteIncludes(ops)
			watchOpts := devpkg.Options{
				Debounce:     500 * time.Millisecond,
				Ignore:       append(append([]string{}, cfg.Dev.Watch.Ignore...), (*watchIgnore)...),
				IncludeFiles: cfg.Dev.Watch.IncludeFiles,
				Included: func(p string) bool {
					w := included.Load()
					if w.files[p] {
						return true
					}
					for _, t := range w.trees {
						if strings.HasPrefix(p, t) && !strings.HasPrefix(filepath.Base(p), ".") {
							return true
						}
					}
					return false
				},
			}
			filesNote := "FILES/ excluded"
			if watchOpts.IncludeFiles {
				filesNote = "FILES/ included"
			}
			if len(watchOpts.Ignore) > 0 {
				filesNote += "; ignoring " + strings.Join(watchOpts.Ignore, ", ")
			}
			fmt.Fprintf(stdout, "[watch] watching %s (.txcl/.json/&include and stack-tree files → draft; colocated .js → rebuild + activate; %s)\n", opsDir, filesNote)
			state := newDevWatchState()
			// .txcl/.json → sticky draft (no auto-activation; activate to publish).
			go func() {
				err := devpkg.WatchOps(ctx, opsDir, watchOpts, func() {
					applyMu.Lock()
					defer applyMu.Unlock()
					fresh, err := readWorkspace(dir)
					if err != nil {
						fmt.Fprintf(stderr, "[watch] walk: %v\n", err)
						return
					}
					noteIncludes(fresh.Ops)
					if err := devApplyToDraft(ctx, fresh, resolved, *staticOnly, state, stdout, stderr); err != nil {
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
				err := devpkg.WatchColocatedComputes(ctx, opsDir, watchOpts, func() {
					applyMu.Lock()
					defer applyMu.Unlock()
					fmt.Fprintln(stdout, "[watch] compute source changed — rebuilding")
					fresh, err := readWorkspace(dir)
					if err != nil {
						fmt.Fprintf(stderr, "[watch] walk: %v\n", err)
						return
					}
					noteIncludes(fresh.Ops)
					if err := devApply(ctx, fresh, resolved, *staticOnly, stdout, stderr); err != nil {
						fmt.Fprintf(stderr, "[watch] compute reload: %v\n", err)
					}
				})
				if err != nil && ctx.Err() == nil {
					fmt.Fprintf(stderr, "[watch] compute: %v\n", err)
				}
			}()
		}

		// A bound Web ABI build is the producer's output: when a rebuild
		// settles, re-apply and ACTIVATE, as for a compute — the build is a
		// deliberate step, with no admin-UI edit to clobber. Works without an
		// OPS/ tree (a pure framework app).
		for _, n := range sortedMapKeys(ws.Bindings) {
			b := ws.Bindings[n]
			fmt.Fprintf(stdout, "[watch] watching %s (the %s stack's Web ABI build → re-apply + activate)\n", b.ABI, n)
			go watchABIBuild(ctx, b.Abs, time.Second, func() {
				applyMu.Lock()
				defer applyMu.Unlock()
				fmt.Fprintf(stdout, "[watch] %s rebuilt — re-applying\n", b.ABI)
				fresh, err := readWorkspace(dir)
				if err != nil {
					fmt.Fprintf(stderr, "[watch] walk: %v\n", err)
					return
				}
				if err := devApply(ctx, fresh, resolved, *staticOnly, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "[watch] %s: %v\n", b.ABI, err)
				}
			})
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
		// front. Suppressed when localhost is already routed (the
		// single-stack auto-bind above, or a bind from a previous run) —
		// telling the user to bind what's bound reads as "it didn't work".
		if localhostStack == "" {
			fmt.Fprintln(stdout, "[txco]   tip: bind a hostname with `txco auth tenant hostnames add localhost --stack <stack>`")
			fmt.Fprintln(stdout, "[txco]        (boot/* resonators still work; hostnames are now the recommended path)")
		} else {
			fmt.Fprintf(stdout, "[txco]   localhost → stack %q (%s routes there)\n", localhostStack, webURL)
		}
		if *dnsHead {
			fmt.Fprintf(stdout, "[txco]   dns head: %s (udp+tcp). create a zone with `txco dns zone create ops.example.com`,\n", devDNSListenAddr)
			fmt.Fprintf(stdout, "[txco]        then `dig @127.0.0.1 -p %s ops.example.com A`\n", strings.TrimPrefix(devDNSListenAddr, "127.0.0.1:"))
		}
		if *lmtpHead {
			// Name the relay the chassis will actually use: an explicit
			// TXCO_MAIL_RELAY_ADDR wins over the dev default (devDefaults
			// is set-if-missing), so printing the constant would lie.
			relay := devMailRelayAddr
			if v, ok := os.LookupEnv("TXCO_MAIL_RELAY_ADDR"); ok && v != "" {
				relay = v
			}
			fmt.Fprintf(stdout, "[txco]   lmtp head: %s → relay %s (run MailHog/Mailpit for the sink)\n", devLMTPListenAddr, relay)
			fmt.Fprintf(stdout, "[txco]        test: swaks --protocol LMTP --server localhost%s --to you@<host>\n", devLMTPListenAddr)
		}
		if *imapHead {
			certPath := filepath.Join(dir, ".txco", "dev", "imap-selfsigned.crt")
			fmt.Fprintf(stdout, "[txco]   imap head: %s (STARTTLS) and %s (IMAPS), self-signed certificate at %s\n", devIMAPListenAddr, devIMAPTLSAddr, certPath)
			fmt.Fprintln(stdout, "[txco]        trust it once so mail clients connect without a warning (macOS):")
			fmt.Fprintf(stdout, "[txco]          security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db %s\n", certPath)
			fmt.Fprintln(stdout, "[txco]        provision with `EXEC \"txco://imap/account\" WITH username = \"you@<bound-host>\"`, then add the account in Mail/Thunderbird:")
			fmt.Fprintln(stdout, "[txco]        server <bound-host> (e.g. pony.local.thanks.computer), port 1993, SSL on")
		}
		if *ippHead {
			certPath := filepath.Join(dir, ".txco", "dev", "web-selfsigned.crt")
			fmt.Fprintf(stdout, "[txco]   ipp head: ipps://ipp.localhost%s/p/<printer> (also ipps://ipp.<bound-host>%s/…), self-signed certificate at %s\n", devIPPTLSAddr[strings.LastIndex(devIPPTLSAddr, ":"):], devIPPTLSAddr[strings.LastIndex(devIPPTLSAddr, ":"):], certPath)
			fmt.Fprintln(stdout, "[txco]        a tenant has printers once it has BOTH an active `_ipp` stack (OPS/_ipp/0/…) and a registered printer:")
			fmt.Fprintln(stdout, "[txco]          EXEC \"txco://ipp/printer\" WITH printer = \"research\", principal = …   then   EXEC \"txco://credential/create\" WITH scopes = [\"ipp:*:*\"]")
			fmt.Fprintln(stdout, "[txco]          (examples/ipp-hello does both: POST /ipp/provision answers the username and a one-time password)")
			fmt.Fprintln(stdout, "[txco]        add it (CUPS trusts a self-signed certificate on first use; -U is the printer's username):")
			fmt.Fprintf(stdout, "[txco]          lpadmin -U <username> -p pony -E -v ipps://ipp.localhost%s/p/research -m everywhere\n", devIPPTLSAddr[strings.LastIndex(devIPPTLSAddr, ":"):])
			fmt.Fprintln(stdout, "[txco]        if the Add Printer dialog refuses the certificate, trust it once (cupsd runs as root, so the System keychain too):")
			fmt.Fprintf(stdout, "[txco]          security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db %s\n", certPath)
			fmt.Fprintf(stdout, "[txco]          sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s\n", certPath)
			fmt.Fprintln(stdout, "[txco]        every request's operation + attribute names are logged (`ipp wire`): that log is the record of what a client sends")
		}
		if *calendarHead {
			fmt.Fprintf(stdout, "[txco]   calendar head: %s/dav/ (CalDAV, Basic auth over plaintext) and /.well-known/caldav; index at .txco/dev/calendar.db\n", webURL)
			fmt.Fprintln(stdout, "[txco]        provision with `EXEC \"txco://calendar/account\" WITH username = \"you@<bound-host>\"` and `txco://calendar/calendar`, then add a CalDAV account in Calendar/Thunderbird:")
			fmt.Fprintln(stdout, "[txco]        server <bound-host> (e.g. pony.local.thanks.computer), the web port, SSL off, path /dav/")
		}
		if *contactsHead {
			fmt.Fprintf(stdout, "[txco]   contacts head: %s/carddav/ (CardDAV, Basic auth over plaintext) and /.well-known/carddav; index at .txco/dev/contacts.db\n", webURL)
			fmt.Fprintln(stdout, "[txco]        provision with `EXEC \"txco://contacts/account\" WITH username = \"you@<bound-host>\"` and `txco://contacts/addressbook`, then add a CardDAV account in Contacts/Thunderbird:")
			fmt.Fprintln(stdout, "[txco]        server <bound-host> (e.g. pony.local.thanks.computer), the web port, SSL off, path /carddav/")
		}
		if *grantHead {
			fmt.Fprintln(stdout, "[txco]   run grants: declare a sandbox in OPS/<stack>/SANDBOXES/<name>.yaml, mint a grant with `EXEC \"txco://delegate/mint\" WITH allow = [\"<name>\"]`,")
			fmt.Fprintln(stdout, "[txco]        open it for a command with `workspace://<ws>/exec WITH grant = <id>, sandbox = \"<name>\"`")
			fmt.Fprintln(stdout, "[txco]        or let the command open one itself: \"$TXCO_BIN\" sandbox <name> -- program")
			fmt.Fprintln(stdout, "[txco]        a secret is handed over only if its policy allows: txco auth tenant secrets policy SECRET --pull any   (or a rule in OPS/_grant)")
			if !*allowLocalWorkspace {
				fmt.Fprintln(stdout, "[txco]        NOTE: no workspace provider is on, so no command can be handed a grant: add --allow-local-workspace")
			}
		}
		if *webdavHead {
			fmt.Fprintf(stdout, "[txco]   webdav head: %s/drive/ (WebDAV, Basic auth over plaintext); index at .txco/dev/drive.db, bytes under .txco/dev/drive/\n", webURL)
			fmt.Fprintln(stdout, "[txco]        provision with `EXEC \"txco://drive/collection\" WITH name = \"docs\"` and `txco://drive/account WITH username = \"you@<bound-host>\", collection = \"docs\"`, then mount it:")
			fmt.Fprintf(stdout, "[txco]        Finder: Connect to Server %s/drive/ — curl: curl -u you@<bound-host> -T file %s/drive/file\n", webURL, webURL)
		}
	} else {
		fmt.Fprintf(stdout, "[txco] dev loop running (Ctrl-C to stop). chassis: %s\n", chassisURL)
		if uiDevURL != "" {
			fmt.Fprintf(stdout, "[txco]   admin UI (HMR): %s\n", uiDevURL)
		}
	}
	// Profile lines: we registered a keyless `dev` profile but deliberately did
	// NOT make it active — the active profile is global, persistent state, so
	// flipping it would hijack prod commands in other shells and outlive this
	// session. Advertise the named selector, then (when you're on a different
	// profile) nudge — don't force — the opt-in that drops the selector entirely.
	if !*noChassis {
		if devProfileAction == auth.DevProfileWrote || devProfileAction == auth.DevProfileCurrent {
			fmt.Fprintf(stdout, "[txco]   profile %q → %s (tenant %s) — e.g. `txco apply %s`\n",
				auth.DevProfileName, chassisURL, auth.DefaultTenantSlug, auth.DevProfileName)
		}
		if active, _ := auth.ReadActiveProfile(); active != auth.DevProfileName && active != auth.ActiveNone {
			fmt.Fprintf(stdout, "[txco]   tip: active profile is %q — run `txco auth profile use %s` so plain\n", active, auth.DevProfileName)
			fmt.Fprintf(stdout, "[txco]        `txco apply` / `txco ui` hit this chassis (then `txco auth profile use %s` to switch back).\n", active)
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
func devApply(ctx context.Context, ws *localWorkspace, resolved ResolvedTarget, staticOnly bool, stdout, stderr io.Writer) error {
	dir := ws.Dir
	out, builtComputes, cerr := resolveOpRefsColocated(ws.Ops, buildOpRefMap(resolved), dir, stderr)
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
	keepSource := chassisKeepsComputeSource(ctx, c, builtComputes, stderr, "dev")
	stacks, allStacks := devStacks(ws, out, staticOnly, stderr)
	totalFiles := 0
	skipped := 0
	for _, stack := range sortedKeys(stacks) {
		// `dev` is a full local mirror (manage="all"): the store-seed packs ride
		// along, so a developer's local VECTORS/+KV/+BLOBS/ are live against the
		// dev chassis without a separate `txco data apply`.
		build, berr := buildStackFiles(dir, stack, stacks[stack], ws.withABI(stack, collectOpts{
			Data: true, Derived: true, KeepSource: keepSource, Built: builtComputes, AllStacks: allStacks,
		}))
		if berr != nil {
			return fmt.Errorf("%s: %w", stack, berr)
		}
		files := build.Files
		localHash := build.Hash()

		// Fast paths against the chassis's current active version:
		//   1. If a saved .txco/<stack>.state.json says we last pulled
		//      v_saved and the chassis's active is now > v_saved, the
		//      admin UI moved ahead of us. Pushing would clobber those
		//      edits, so warn loudly and skip this stack.
		//   2. If the chassis's active manifest already matches local,
		//      there's nothing to do — skip the version churn.
		// Anything else falls through to the existing create/push/
		// activate path.
		var expectedActive *int64
		st, getErr := c.GetStack(ctx, stack)
		if getErr == nil && st != nil {
			expectedActive = expectedActiveFor(st) // ref CAS for the activate below
		}
		if getErr == nil && st != nil && st.ActiveVersion != nil {
			active := *st.ActiveVersion
			saved, _ := state.Load(dir, stack, stateKeyFor(c))
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
				_ = state.Save(dir, stack, stateKeyFor(c), state.State{
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
		if err := build.ensureResident(ctx, c, stdout, stderr); err != nil {
			return fmt.Errorf("%s: %w", stack, err)
		}
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
		if _, err := c.ActivateWith(ctx, stack, versionNumber, client.ActivateOpts{ExpectedActive: expectedActive}); err != nil {
			return fmt.Errorf("%s: activate v%d: %w", stack, versionNumber, err)
		}
		// Persist the new pointer so a future restart will skip when
		// nothing changed, and so the divergence check has a baseline.
		if err := state.Save(dir, stack, stateKeyFor(c), state.State{
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
	drafts map[string]int64  // stack name → current draft version_number
	fps    map[string]string // stack name → last-pushed source fingerprint
}

func newDevWatchState() *devWatchState {
	return &devWatchState{drafts: map[string]int64{}, fps: map[string]string{}}
}

// recordDevURLs queries the dev chassis for each stack's structured
// hostname, writes stack→URL to .txco/dev/urls.json (so a separate
// `txco status` can surface the dev URL), and prints the reachable URLs.
// Best-effort: every failure is non-fatal — the URLs are a convenience.
// No-op without a spawned chassis (webURL == "").
func recordDevURLs(ctx context.Context, resolved ResolvedTarget, dir, webURL string, stdout io.Writer) {
	if webURL == "" {
		return
	}
	c := client.New(resolved.AsClientTarget())
	hosts, err := c.ListHostnames(ctx, false)
	if err != nil {
		return
	}
	best := map[string]client.Hostname{}
	for _, h := range hosts {
		if h.Stack == "" || h.RevokedAt != "" {
			continue
		}
		if cur, ok := best[h.Stack]; !ok || betterHostname(h, cur) {
			best[h.Stack] = h
		}
	}
	urls := map[string]string{}
	for stack, h := range best {
		if u := devStackURL(webURL, h.Hostname); u != "" {
			urls[stack] = u
		}
	}
	if len(urls) == 0 {
		return
	}
	if err := writeDevURLs(dir, urls); err != nil {
		fmt.Fprintf(stdout, "[txco] warn: record dev urls: %v\n", err)
	}
	stacks := make([]string, 0, len(urls))
	for s := range urls {
		stacks = append(stacks, s)
	}
	sort.Strings(stacks)
	fmt.Fprintln(stdout, "[txco] stack URLs (this dev chassis):")
	for _, s := range stacks {
		fmt.Fprintf(stdout, "[txco]   %s → %s\n", s, urls[s])
	}
}

// devAutoBindLocalhost binds plain `localhost` to the workspace's stack
// when — and only when — that choice is unambiguous: exactly one
// non-underscore stack in the walked bundle. Underscore stacks (_llm,
// _cron, _sys/…) are inlet/system surfaces, not hostname-routable web
// stacks, so they don't count and don't disqualify.
//
// Returns the stack `localhost` routes to after this call ("" = not
// bound): the freshly-bound stack, or the existing binding's stack when
// a previous run (or the user) already decided — an existing row is
// respected, never rewritten. Best-effort like recordDevURLs: any
// admin-API failure degrades to the manual-bind tip, not an error.
func devAutoBindLocalhost(ctx context.Context, resolved ResolvedTarget, dir string, ops []bundle.Op, webURL string, stdout io.Writer) string {
	if webURL == "" {
		return ""
	}
	// A workspace that customized the boot pipeline owns its routing —
	// e.g. mcp-server's path-gated auto-route at boot/75 DEPENDS on
	// unrouted requests (non-matching paths must fall through to the
	// 404), and binding localhost would shadow that design. Checked on
	// the filesystem, NOT via `ops`: bundle.Walk excludes the _sys tree.
	if workspaceHasCustomBootScope(dir) {
		return ""
	}
	stacks := map[string]bool{}
	for _, op := range ops {
		if op.Stack == "" || strings.HasPrefix(op.Stack, "_") {
			continue
		}
		stacks[op.Stack] = true
	}
	if len(stacks) != 1 {
		return ""
	}
	var stack string
	for s := range stacks {
		stack = s
	}
	c := client.New(resolved.AsClientTarget())
	hosts, err := c.ListHostnames(ctx, false)
	if err != nil {
		return ""
	}
	for _, h := range hosts {
		if h.Hostname == "localhost" && h.RevokedAt == "" {
			if h.Stack != "" {
				return h.Stack // already routed; suppress the tip, change nothing
			}
			return "" // claimed but unattached — the user's arrangement; leave it
		}
	}
	if _, err := c.AddHostname(ctx, client.AddHostnameRequest{Hostname: "localhost", Stack: stack}); err != nil {
		fmt.Fprintf(stdout, "[txco] warn: auto-bind localhost → %s: %v\n", stack, err)
		return ""
	}
	fmt.Fprintf(stdout, "[txco] localhost auto-bound → stack %q (single-stack workspace; `txco auth tenant hostnames rm localhost` to undo)\n", stack)
	return stack
}

// workspaceHasCustomBootScope reports whether OPS/_sys/boot contains a
// scope directory outside the vendored canon {0 detect/healthz,
// 50 static, 100 route, 1000 notfound} — the "operator hook" band. A
// hook there (mcp-server's 75, playground's 25) means the workspace
// routes on its own terms. Labeled scope dirs ("75_auto-route") count by
// their numeric prefix, same as the OPS walker.
func workspaceHasCustomBootScope(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "OPS", "_sys", "boot"))
	if err != nil {
		return false // no vendored boot at all — nothing custom
	}
	canon := map[int]bool{0: true, 50: true, 100: true, 1000: true}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if i := strings.IndexByte(name, '_'); i > 0 {
			name = name[:i]
		}
		n, aerr := strconv.Atoi(name)
		if aerr != nil {
			continue // not a scope directory
		}
		if !canon[n] {
			return true
		}
	}
	return false
}

// devApplyToDraft is the watcher's push path. It maintains one
// sticky draft per stack and PUTs the latest file set to it on every
// fire — no activation. If the tracked draft has been activated out
// from under us (status flipped to 'superseded'), the server returns
// 409 version_not_draft and we transparently create a fresh draft
// and retry once.
func devApplyToDraft(ctx context.Context, ws *localWorkspace, resolved ResolvedTarget, staticOnly bool, state *devWatchState, stdout, stderr io.Writer) error {
	dir := ws.Dir
	out, builtComputes, cerr := resolveOpRefsColocated(ws.Ops, buildOpRefMap(resolved), dir, stderr)
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
	keepSource := chassisKeepsComputeSource(ctx, c, builtComputes, stderr, "dev")
	stacks, allStacks := devStacks(ws, out, staticOnly, stderr)
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, stack := range sortedKeys(stacks) {
		stackDir := filepath.Join(dir, "OPS", stack)
		// Skip stacks whose source didn't change since our last push — one edit
		// re-pushes one stack, not all 60. The fingerprint is cheap: op contents
		// (already in memory from the walk) + a STAT-only walk of FILES/, VECTORS/,
		// KV/ (no asset content read). First fire after startup has no recorded
		// fingerprint, so every stack syncs once, then incrementally.
		abiDir := ""
		if d := ws.ABI[stack]; d != nil {
			abiDir = d.Path
		}
		fp, ferr := stackSourceFingerprint(stacks[stack], stackDir, abiDir)
		if ferr != nil {
			return fmt.Errorf("%s: fingerprint: %w", stack, ferr)
		}
		if state.fps[stack] == fp {
			continue
		}

		// Full local mirror — include store-seed packs (see devApply).
		build, berr := buildStackFiles(dir, stack, stacks[stack], ws.withABI(stack, collectOpts{
			Data: true, Derived: true, KeepSource: keepSource, Built: builtComputes, AllStacks: allStacks,
		}))
		if berr != nil {
			return fmt.Errorf("%s: %w", stack, berr)
		}
		if err := build.ensureResident(ctx, c, stdout, stderr); err != nil {
			return fmt.Errorf("%s: %w", stack, err)
		}
		files := build.Files
		if n, ok := state.drafts[stack]; ok {
			if _, err := c.PutDraftFiles(ctx, stack, n, files); err == nil {
				state.fps[stack] = fp
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
		state.fps[stack] = fp
		fmt.Fprintf(stdout, "[watch] %s new draft v%d (%d files) — run `txco activate %s` to publish\n",
			stack, n, len(files), stack)
	}
	return nil
}

// stackSourceFingerprint cheaply fingerprints a stack's on-disk source so the
// watch loop can skip stacks that didn't change. It hashes the op files
// (opsToFiles is pure in-memory — the .txcl text is already loaded) plus a
// STAT-only walk (path + size + mtime, never reading content) of the stack's
// FILES/, VECTORS/, and KV/ asset trees. Mirrors the dirs collectFileAssets /
// collectStorePacks read, so any change they'd pick up changes the fingerprint
// — without paying their per-file content reads on every watcher fire.
func stackSourceFingerprint(ops []bundle.Op, stackDir, abiDir string) (string, error) {
	h := sha256.New()
	// A bound Web ABI build: a stat-only walk of the whole build directory.
	if abiDir != "" {
		fp, err := treeFingerprint(abiDir)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "abi\x00%s\n", fp)
	}
	// Op half: content-hash the op files (cheap; small UTF-8 text in memory).
	for _, f := range opsToFiles(ops) { // already sorted by Path
		fmt.Fprintf(h, "op\x00%s\x00%s\n", f.Path, f.Content)
	}
	// Asset half: stat-only walk of the same trees the collectors read.
	for _, top := range []string{"FILES", storeseed.DirVectors, storeseed.DirKV, storeseed.DirCalendars, storeseed.DirContacts, storeseed.DirBlobs, storeseed.DirSources, outlet.Dir, sandbox.Dir, capdecl.Dir, dataset.Dir} {
		treeDir := filepath.Join(stackDir, top)
		info, err := os.Stat(treeDir)
		if err != nil || !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(treeDir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			base := filepath.Base(p)
			if d.IsDir() {
				if p != treeDir && strings.HasPrefix(base, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || strings.HasPrefix(base, ".") {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, rerr := filepath.Rel(stackDir, p)
			if rerr != nil {
				return rerr
			}
			fmt.Fprintf(h, "f\x00%s\x00%d\x00%d\n", filepath.ToSlash(rel), fi.Size(), fi.ModTime().UnixNano())
			return nil
		})
		if walkErr != nil {
			return "", walkErr
		}
	}
	// Tree half: a stack whose ops name $TXCO_STACK_DIR ships its own files
	// (stackTreeRows), so any of them changing is a change. Stat-only again;
	// a nested stack's edits re-push this one too, which costs one no-op.
	if usesStackDir(ops) {
		walkErr := filepath.WalkDir(stackDir, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if p != stackDir && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			fi, ferr := d.Info()
			if ferr != nil {
				return ferr
			}
			rel, rerr := filepath.Rel(stackDir, p)
			if rerr != nil {
				return rerr
			}
			fmt.Fprintf(h, "t\x00%s\x00%d\x00%d\x00%v\n", filepath.ToSlash(rel), fi.Size(), fi.ModTime().UnixNano(), fi.Mode()&0o111 != 0)
			return nil
		})
		if walkErr != nil {
			return "", walkErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// devHeads is the optional heads `txco dev` can start beside cron, web,
// admin and websocket. All off by default — most dev workflows use only
// those four, and the extra binds otherwise cause spurious "port in use"
// failures on machines running other things there.
type devHeads struct {
	TCP, DNS, LMTP, Scheduled, Source, IMAP, Calendar, Contacts, WebDAV, IPP, State bool
	// Grant is the launcher's socket: how a command opens a sandbox itself.
	Grant bool
}

// chassisOpts is what startChassis is asked for. A new switch is one field
// here, never another parameter.
type chassisOpts struct {
	// Workspace is the directory the chassis runs in; its state lands under
	// <Workspace>/.txco/dev.
	Workspace string
	// AdminAddr and WebAddr override the canonical :8081 and :8080.
	AdminAddr, WebAddr string
	Heads              devHeads
	// AllowLocalWorkspace turns on the local workspace provider, which runs
	// commands as this uid with no isolation.
	AllowLocalWorkspace bool
	// LocalExec, with AllowLocalWorkspace, is the command prefix the local
	// provider hands every command to (chassis/workspace/local, EnvExec).
	LocalExec      string
	Verbose        bool
	Stdout, Stderr io.Writer
	// Started collects every process spawned, for teardown; Out receives
	// the chassis's own.
	Started *[]*devpkg.Process
	Out     **devpkg.Process
	// Executable is the txco binary to run as the chassis; "" means this
	// process's own (os.Executable). Tests point it at a built binary.
	Executable string
}

// chassisAddrs is where the spawned chassis listens, once defaults and the
// parent environment have been applied.
type chassisAddrs struct {
	Admin, Web, TCP, DNS string
}

// startChassis spawns a chassis subprocess pointed at a per-workspace
// temp DB. Returns the admin URL and the web URL (the curlable one
// developers actually hit during dev).
func startChassis(ctx context.Context, o chassisOpts) (adminURL, webURL string, err error) {
	executable := o.Executable
	if executable == "" {
		if executable, err = os.Executable(); err != nil {
			return "", "", fmt.Errorf("locate self: %w", err)
		}
	}

	devDir := filepath.Join(o.Workspace, ".txco", "dev")
	dbDir := filepath.Join(devDir, "db")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", dbDir, err)
	}

	// txco dev uses the canonical chassis ports so the URLs are
	// predictable from the docs without reading the boot log. Each
	// port is fail-fast: if something's already bound, surface a clear
	// error rather than silently picking a random port (which leaves
	// users curling the wrong address).
	adminAddr := o.AdminAddr
	if adminAddr == "" {
		adminAddr = ":8081"
	}
	if err := requirePortFree(adminAddr, "admin API"); err != nil {
		return "", "", err
	}
	webAddr := o.WebAddr
	if webAddr == "" {
		webAddr = ":8080"
	}
	if err := requirePortFree(webAddr, "web inlet"); err != nil {
		return "", "", err
	}
	// TXCO_TCP_LISTEN_ADDRS in the parent env replaces the :5050 default
	// (e.g. "irc=127.0.0.1:6697;self-signed" for a TLS/SNI listener). It
	// is a full listener spec, not an address, so the free-port precheck
	// is skipped; the chassis itself fails loudly on a bind conflict.
	tcpAddr, tcpOverridden := ":5050", false
	if v := strings.TrimSpace(os.Getenv("TXCO_TCP_LISTEN_ADDRS")); v != "" {
		tcpAddr, tcpOverridden = v, true
	}
	if o.Heads.TCP && !tcpOverridden {
		if err := requirePortFree(tcpAddr, "TCP inlet"); err != nil {
			return "", "", err
		}
	}
	dnsAddr := devDNSListenAddr
	if o.Heads.DNS {
		if err := requirePortFree(dnsAddr, "DNS inlet"); err != nil {
			return "", "", err
		}
	}
	if o.Heads.IPP {
		if err := requirePortFree(devIPPTLSAddr, "IPPS (web TLS) listener"); err != nil {
			return "", "", err
		}
	}
	if o.Heads.IMAP {
		if err := requirePortFree(devIMAPListenAddr, "IMAP head"); err != nil {
			return "", "", err
		}
		if err := requirePortFree(devIMAPTLSAddr, "IMAPS head"); err != nil {
			return "", "", err
		}
	}
	if o.Heads.LMTP {
		if err := requirePortFree(devLMTPListenAddr, "LMTP inlet"); err != nil {
			return "", "", err
		}
	}

	// Locate the schema dir relative to the workspace; fall back to a
	// known repo-relative path.
	schemaDir := findSchemaDir(o.Workspace)

	env, _ := chassisEnv(o, chassisAddrs{Admin: adminAddr, Web: webAddr, TCP: tcpAddr, DNS: dnsAddr},
		devDir, schemaDir, os.LookupEnv)

	// Disable basic auth in dev (chassis emits a WARN at boot).

	tcpDesc := "off"
	if o.Heads.TCP {
		tcpDesc = tcpAddr
	}
	fmt.Fprintf(o.Stdout, "[txco] starting chassis (admin=%s, web=%s, tcp=%s, db=%s)\n", adminAddr, webAddr, tcpDesc, dbDir)
	p, err := devpkg.Spawn(ctx, devpkg.SpawnConfig{
		Name: "chassis",
		// `exec`, so the chassis replaces the shell Spawn starts and IS the
		// process that is waited on. With the shell in front, stopping ended
		// the shell at once, txco dev returned, and the chassis — its log
		// pipe gone — died in the middle of its own shutdown: nothing it
		// holds open was closed, and the grant socket was left in its
		// directory.
		Cmd: "exec " + shellEscape(executable) + " serve",
		Out: o.Stdout,
		Env: env,
	})
	if err != nil {
		return "", "", fmt.Errorf("spawn chassis: %w", err)
	}
	*o.Started = append(*o.Started, p)
	*o.Out = p

	adminURL = "http://localhost" + adminAddr
	webURL = "http://localhost" + webAddr
	if err := devpkg.WaitHealthy(ctx, adminURL+"/healthz", 30*time.Second, 500*time.Millisecond); err != nil {
		return "", "", fmt.Errorf("chassis health: %w", err)
	}
	return adminURL, webURL, nil
}

// chassisEnv is the environment of the chassis `txco dev` spawns, and the
// heads it starts. parent looks a variable up in the environment txco dev
// itself was started with (os.LookupEnv): the dev defaults are
// set-if-missing, so whatever the developer exported wins.
//
// It spawns and binds nothing, so what a flag does to the chassis can be
// tested without starting one.
func chassisEnv(o chassisOpts, a chassisAddrs, devDir, schemaDir string, parent func(string) (string, bool)) (env, heads []string) {
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
	filecasDir := filepath.Join(devDir, "filecas")
	feedDir := filepath.Join(devDir, "feed")
	secretKeyPath := filepath.Join(devDir, "secrets", "txco-master.key")
	vectorDBPath := filepath.Join(devDir, "vector", "vector.db")

	// Hard-coded path overrides: workspace-scoped state has to land
	// under .txco/dev/, otherwise chassis defaults would litter the
	// repo with chassis/data/*. These are always set, never overridden
	// by the parent env.
	env = []string{
		"TXCO_ADMIN_ADDR=" + a.Admin,
		"TXCO_WEB_ADDR=" + a.Web,
		"TXCO_DB_ROOT_DIR=" + filepath.Join(devDir, "db"),
		"TXCO_KVSTORE_ADDRS=" + kvDir,
		"TXCO_LOG_OPS_DIR=" + logsDir,
		"TXCO_ADMIN_ROOT_DIR=" + adminStaticDir,
		"TXCO_DOCKER_TMP=" + dockerTmpDir,
		"TXCO_REPOSTORE_FILE_DIR=" + repoDir,
		"TXCO_CONTINUATION_STORE_FILE_DIR=" + continuationsDir,
		"TXCO_ARTIFACT_STORE_FILE_DIR=" + artifactsDir,
		"TXCO_FILECAS_STORE_FILE_DIR=" + filecasDir,
		"TXCO_FEED_SOURCE_FILE_DIR=" + feedDir,
		"TXCO_SECRET_MASTER_KEY=" + secretKeyPath,
		"TXCO_VECTOR_DB_PATH=" + vectorDBPath,
		"TXCO_SEARCH_PATH=" + filepath.Join(devDir, "search"),
		"TXCO_NOTEBOOK_DB_PATH=" + filepath.Join(devDir, "notebook.db"),
	}

	// Personality set. We pin it to cron+web+admin and add the heavier
	// heads (tcp, dns, lmtp) only when opted in via --tcp / --dns /
	// --lmtp. Each head's *ListenAddrs env is set only when that head is
	// on (otherwise the chassis would bind the head's default port per
	// its own default); pinning Personalities suppresses an un-opted head
	// entirely (see the strings.Contains gate in each personality's
	// Start).
	// websocket rides the web listener (no port of its own) and does nothing
	// until a stack accepts an upgrade, so dev turns it on by default; prod
	// opts in via TXCO_PERSONALITIES.
	heads = []string{"cron", "web", "admin", "websocket"}
	if o.Heads.TCP {
		heads = append(heads, "tcp")
		env = append(env, "TXCO_TCP_LISTEN_ADDRS="+a.TCP)
	}
	if o.Heads.DNS {
		heads = append(heads, "dns")
		env = append(env, "TXCO_DNS_LISTEN_ADDRS="+a.DNS)
	}
	if o.Heads.LMTP {
		heads = append(heads, "lmtp")
		env = append(env, "TXCO_LMTP_LISTEN_ADDRS="+devLMTPListenAddr)
	}
	if o.Heads.Scheduled {
		heads = append(heads, "scheduled")
		env = append(env, "TXCO_SCHEDULED_DB_PATH="+filepath.Join(devDir, "scheduled.db"))
	}
	if o.Heads.State {
		heads = append(heads, "state")
		env = append(env, "TXCO_STATE_DB_PATH="+filepath.Join(devDir, "state.db"))
	}
	if o.Heads.Source {
		// The source poller reads declared sources from the shared runtime DB
		// (the dev SQLite runtime, always open), materializes each mailbox
		// password from the secret store, and dials out through the egress
		// guard — all already wired in dev. No extra env: the personality is
		// the whole switch.
		heads = append(heads, "source")
	}
	if o.Heads.IMAP {
		heads = append(heads, "imap")
		env = append(env, "TXCO_IMAP_LISTEN_ADDRS="+devIMAPListenAddr)
		env = append(env, "TXCO_IMAP_TLS_ADDRS="+devIMAPTLSAddr)
		env = append(env, "TXCO_IMAP_DB_PATH="+filepath.Join(devDir, "imap.db"))
	}
	if o.Heads.Calendar {
		heads = append(heads, "calendar")
		env = append(env, "TXCO_CALENDAR_DB_PATH="+filepath.Join(devDir, "calendar.db"))
	}
	if o.Heads.Contacts {
		heads = append(heads, "contacts")
		env = append(env, "TXCO_CONTACTS_DB_PATH="+filepath.Join(devDir, "contacts.db"))
	}
	if o.Heads.WebDAV {
		heads = append(heads, "webdav")
		env = append(env, "TXCO_DRIVE_DB_PATH="+filepath.Join(devDir, "drive.db"))
		env = append(env, "TXCO_DRIVE_OBJECTS_FILE_DIR="+filepath.Join(devDir, "drive"))
	}
	if o.Heads.IPP {
		heads = append(heads, "ipp")
		env = append(env, "TXCO_WEB_TLS_ADDR="+devIPPTLSAddr)
		env = append(env, "TXCO_IPP_DB_PATH="+filepath.Join(devDir, "ipp.db"))
	}
	if o.Heads.Grant {
		// No path of its own: the chassis puts the socket in the system's
		// temp dir, named for this workspace's database directory. A socket
		// under .txco/dev could run past the 100 bytes a socket's path may be.
		heads = append(heads, "grant")
	}
	env = append(env, "TXCO_PERSONALITIES="+strings.Join(heads, ","))
	if o.AllowLocalWorkspace {
		// Unconditional (NOT a devDefault): the local provider runs
		// commands as this uid with no isolation, so it is never implied
		// by the dev posture — only by this flag, and only for this run.
		env = append(env, "TXCO_WORKSPACE_PROVIDER=local")
		env = append(env, "TXCO_WORKSPACE_ALLOW_LOCAL=true")
		env = append(env, "TXCO_WORKSPACE_LOCAL_ROOT="+filepath.Join(devDir, "workspaces"))
		if strings.TrimSpace(o.LocalExec) != "" {
			// The provider's own setting: its commands go to this prefix
			// program, on another machine, instead of running here.
			env = append(env, "TXCO_WORKSPACE_LOCAL_EXEC="+strings.TrimSpace(o.LocalExec))
		}
	}

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
		// txco://drive/sign mints its URLs on the dev web inlet, by the name
		// everything else in dev uses — not this machine's hostname, which a
		// laptop often cannot resolve.
		"TXCO_SIGNED_URL_BASE": "http://localhost" + a.Web,
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
		// Same rationale for the outbound OP dial path: dev rules
		// legitimately EXEC http://localhost:… mock services and call
		// each other over loopback. The serve default is `private`
		// (SSRF-safe); dev opts into `open`. Set-if-missing, so
		// `TXCO_EGRESS_POLICY=private txco dev` re-enables the guard.
		"TXCO_EGRESS_POLICY": "open",
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
		"TXCO_SYSTEM_OPSTACKS_DIR":   o.Workspace,
		"TXCO_SYSTEM_OPSTACKS_WATCH": "true",
	}
	// DNS synthesis infra defaults (only when the head is on). Edge =
	// loopback so synthesized A records point where the dev chassis
	// actually serves; MX = localhost (the LMTP head); placeholder NS
	// names. Set-if-missing like the rest, so
	// `TXCO_DNS_EDGE_IPS=… txco dev --dns` overrides. The chassis serve
	// default for these stays empty (operator must configure in prod).
	if o.Heads.DNS {
		devDefaults["TXCO_DNS_NAMESERVERS"] = "ns1.localhost,ns2.localhost"
		devDefaults["TXCO_DNS_EDGE_IPS"] = "127.0.0.1"
		devDefaults["TXCO_DNS_MX_HOST"] = "localhost"
	}
	// Mail-loop defaults (only when the head is on). The outbound relay
	// points at the conventional local sink (MailHog/Mailpit on :1025,
	// plaintext) so a dev chassis can never deliver real mail by
	// accident — overriding TXCO_MAIL_RELAY_ADDR is the deliberate act.
	// And when the workspace carries an ingress.yaml (the local stand-in
	// for minted hostnames: recipient wildcards / http hosts → stacks),
	// load it. Set-if-missing like the rest.
	// IMAP dev default: LOGIN over the plaintext loopback listener. The
	// serve default stays false (a real deployment terminates TLS at the
	// edge or sets --imap-tls-addrs).
	if o.Heads.IMAP {
		devDefaults["TXCO_IMAP_INSECURE_AUTH"] = "true"
		devDefaults["TXCO_IMAP_SELF_SIGNED"] = "true"
	}
	// Calendar dev default: Basic auth over the plaintext web port. The
	// serve default stays false (a real deployment terminates TLS at the
	// edge and forwards X-Forwarded-Proto, or sets --web-tls-addr).
	if o.Heads.Calendar {
		devDefaults["TXCO_CALENDAR_INSECURE_AUTH"] = "true"
	}
	if o.Heads.WebDAV {
		devDefaults["TXCO_DRIVE_INSECURE_AUTH"] = "true"
	}
	// IPP dev defaults: the HTTPS listener serves a self-signed certificate
	// kept under .txco/dev (there is no dns head or CA in dev — without this
	// the ACME manager would be built and every handshake would fail), the
	// plain web port also accepts Basic auth (so `ipp://…:8080` works for
	// ipptool/curl), and the wire log is on: recording what a print client
	// actually sends is what dev mode is FOR. Serve defaults stay off.
	if o.Heads.IPP {
		devDefaults["TXCO_WEB_TLS_SELF_SIGNED"] = "true"
		devDefaults["TXCO_WEB_TLS_SELF_SIGNED_CERT_DIR"] = devDir
		devDefaults["TXCO_IPP_INSECURE_AUTH"] = "true"
		devDefaults["TXCO_IPP_WIRE_DEBUG"] = "true"
	}
	if o.Heads.Contacts {
		devDefaults["TXCO_CONTACTS_INSECURE_AUTH"] = "true"
	}
	if o.Heads.LMTP {
		devDefaults["TXCO_MAIL_RELAY_ADDR"] = devMailRelayAddr
		devDefaults["TXCO_MAIL_RELAY_TLS"] = "none"
		ing := filepath.Join(o.Workspace, "ingress.yaml")
		if _, statErr := os.Stat(ing); statErr == nil {
			devDefaults["TXCO_INGRESS_CONFIG"] = ing
		}
	}
	for k, v := range devDefaults {
		if _, set := parent(k); !set {
			env = append(env, k+"="+v)
		}
	}

	// --verbose wins over both default and parent env. Appended last
	// so it overrides any prior TXCO_LOG_LEVEL in the env slice.
	if o.Verbose {
		env = append(env, "TXCO_LOG_LEVEL=debug")
	}

	// Point the spawned chassis at a dev-scoped master-key path so
	// the file lands under .txco/dev/ (gitignored) instead of the
	// default ./chassis/data/secrets/. The chassis's boot path
	// auto-mints on first run via secrets.LoadOrMintFileMasterKey —
	// same UX as the runtime DB. Honors a parent
	// TXCO_SECRET_MASTER_KEY override.
	if _, set := parent("TXCO_SECRET_MASTER_KEY"); !set {
		keyPath := filepath.Join(devDir, "secrets", "txco-dev-master.key")
		env = append(env, "TXCO_SECRET_MASTER_KEY="+keyPath)
	}
	if schemaDir != "" {
		env = append(env, "TXCO_DB_SCHEMA_DIR="+schemaDir)
	}
	return env, heads
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
