// Package cli holds the txco subcommand surface — the developer-facing
// rule-authoring loop. Subcommands live in their own files (init.go,
// apply.go, diff.go); this file is just the dispatcher.
//
// The dispatcher is invoked from chassis/main.go before the server-mode
// config loader, so subcommands can declare their own flag namespaces
// without colliding with the server's --web-addr et al.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/cloud"
	opcli "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/clientcmd"
)

// BuildInfo carries the ldflag-injected build identity (set in
// cmd/txco/main.go, stamped at build time by chassis/Makefile or
// .github/workflows/release.yml). app.Run assigns Build before calling
// Dispatch so the help screen + the `version` subcommand can read it
// without threading it through every signature.
type BuildInfo struct {
	Version        string
	CommitId       string
	BuildTimestamp string
	// InstallMethod is the build origin stamped via ldflag: "source"
	// (Makefile / unstamped dev builds) or "release" (the GitHub release
	// build). The update package refines it at runtime — see
	// chassis/cli/update. Empty is treated as "source".
	InstallMethod string
	// Chassis is the embedded open-core pin for a distribution that wraps
	// the chassis as a dependency (e.g. the txco-saas overlay stamps the
	// core pseudo-version here, while Version/CommitId describe the overlay
	// build itself). Empty for the open-core binary, where Version/CommitId
	// already are the chassis build.
	Chassis string
}

// Build is the process-wide build identity. Zero values are tolerated —
// the help line is suppressed and the version JSON simply contains empty
// strings; for real binaries app.Run sets it before any CLI dispatch.
var Build BuildInfo

// Dispatch routes a `txco <subcommand> ...` invocation to the right command.
// Returns ok=true if a subcommand was dispatched (caller should exit with the
// returned status code) and ok=false if the args don't name a known subcommand
// (caller should fall through to server-mode boot).
//
// args is typically os.Args; args[0] is the program name, args[1] is the
// subcommand. `serve` is recognized but treated as a no-op so the server-mode
// boot can run.
//
// Bare `txco` prints help (Stripe/gcloud-style) — starting the server now
// requires the explicit `txco serve`. Server-mode flags (e.g.
// `txco --web-addr=:8080`) still fall through for back-compat.
func Dispatch(args []string, stdout, stderr io.Writer) (status int, ok bool) {
	if len(args) < 2 {
		printUsage(stdout)
		return 0, true
	}
	cmd := args[1]
	rest := args[2:]

	switch cmd {
	case "serve":
		return 0, false
	case "init":
		return runInit(rest, stdout, stderr), true
	case "apply":
		return jsonErrWrap(rest, stdout, stderr, runApply), true
	case "diff":
		return jsonErrWrap(rest, stdout, stderr, runDiff), true
	case "status":
		return jsonErrWrap(rest, stdout, stderr, runStatus), true
	case "pull":
		return jsonErrWrap(rest, stdout, stderr, runPull), true
	case "draft":
		return jsonErrWrap(rest, stdout, stderr, runDraft), true
	case "push":
		// Deploy a single stack (create draft + activate) — the inverse of
		// `pull`. Distinct from `apply` (whole workspace) and `draft`
		// (stage without activating). See chassis/cli/apply.go.
		return jsonErrWrap(rest, stdout, stderr, runPush), true
	case "activate":
		return jsonErrWrap(rest, stdout, stderr, runActivate), true
	case "versions":
		return jsonErrWrap(rest, stdout, stderr, runVersions), true
	case "edit":
		return runEdit(rest, stdout, stderr), true
	case "dev":
		return runDev(rest, stdout, stderr), true
	case "demo":
		return runDemo(rest, stdout, stderr), true
	case "trace":
		return runTrace(rest, stdout, stderr), true
	case "snapshot":
		return runSnapshot(rest, stdout, stderr), true
	case "auth":
		return auth.Dispatch(rest, stdout, stderr), true
	case "whoami":
		// Top-level convenience alias for `auth whoami` — confirm the
		// chassis's view of the active identity. Users reflexively type
		// `txco whoami`, so route it instead of erroring as unknown.
		return auth.Dispatch(append([]string{"whoami"}, rest...), stdout, stderr), true
	case "ui":
		// Top-level convenience alias for `auth login` — signs a browser
		// session with the active profile's key and opens the chassis admin
		// UI already authenticated. Surfaced at the top level (and in help)
		// because users look for "the admin interface", not a login verb
		// nested under `auth`. Takes the same flags (--profile, --tenant,
		// --url, --no-open, --label).
		return auth.Dispatch(append([]string{"login"}, rest...), stdout, stderr), true
	case "login":
		// Cloud account sign-in (OAuth against the thanks-computer cloud) —
		// distinct from `auth login`, which mints a chassis admin browser
		// session. Hand the cloud package our version so it can warn (warn-
		// only) after login if this CLI is below the chassis's minimum.
		cloud.ClientVersion = Build.Version
		return cloud.Dispatch(append([]string{"login"}, rest...), stdout, stderr), true
	case "logout":
		return cloud.Dispatch(append([]string{"logout"}, rest...), stdout, stderr), true
	case "cloud":
		cloud.ClientVersion = Build.Version
		return cloud.Dispatch(rest, stdout, stderr), true
	case "op":
		return opcli.Dispatch(rest, stdout, stderr), true
	case "install":
		return runInstall(rest, stdout, stderr), true
	case "package":
		return runPackage(rest, stdout, stderr), true
	case "packages":
		// Top-level convenience alias for `package list`.
		return runList(rest, stdout, stderr), true
	case "mcp":
		return runMcp(rest, stdout, stderr), true
	case "config":
		return runConfig(rest, stdout, stderr), true
	case "dns":
		return runDNS(rest, stdout, stderr), true
	case "cron":
		return runCron(rest, stdout, stderr), true
	case "room":
		// `thanks` is this command under another name (argv[0] dispatch in
		// chassis/app.roomAlias). A room message becomes a normal
		// @src=="room" event — see chassis/cli/room.go.
		return runRoom(rest, stdout, stderr), true
	case "admin":
		return runAdmin(rest, stdout, stderr), true
	case "update":
		// Check for a newer txco CLI release. `txco upgrade` performs it.
		return runCLIUpdate(rest, stdout, stderr), true
	case "upgrade":
		// Upgrade the txco CLI binary itself (self-update for self-managed
		// installs; brew/source guidance otherwise). Distinct from
		// `txco package upgrade`, which re-resolves installed packages.
		return runCLIUpgrade(rest, stdout, stderr), true
	case "doctor":
		// Diagnose local setup + chassis reachability (home/profile/keys/
		// signer/version sync). Diagnose-only; see chassis/cli/doctor.go.
		return runDoctor(rest, stdout, stderr), true
	case "completion":
		// Emit a shell completion script. Boring v1: command + flag
		// names only; see chassis/cli/completion.go.
		return runCompletion(rest, stdout, stderr), true
	case "plugin":
		// List external txco-<name> CLI plugins (git/kubectl convention).
		// The exec-on-unknown-subcommand path lives in the default arm.
		return runPlugin(rest, stdout, stderr), true
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0, true
	case "version", "--version", "-v":
		return runVersion(stdout), true
	default:
		// Unknown built-in → first an overlay-registered client command
		// (chassis/clientcmd; the cloud overlay's blank import populates it,
		// open core registers none), then a local external plugin `txco-<cmd>`
		// (git/kubectl convention), then the zero-install path: forward it to
		// the connected chassis's /v1/cli (a silent signed request that the
		// server either runs or 404s). Built-ins above always win.
		if h, ok := clientcmd.Lookup(cmd); ok {
			return runClientCmd(h, rest, stdout, stderr), true
		}
		if status, ok := execPlugin(cmd, rest, stdout, stderr); ok {
			return status, true
		}
		if status, ok := forwardToServer(cmd, rest, stdout, stderr); ok {
			return status, true
		}
		// A bare word (not a flag) that isn't a known subcommand is
		// almost certainly a typo — `txco whoami` instead of `txco
		// auth whoami`, etc. Falling through to server boot would be
		// surprising. Flags pass through unchanged so server-mode
		// arguments like `--web-addr=:8080` still work.
		if !strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(stderr, "txco: unknown subcommand %q\n\n", cmd)
			printUsage(stderr)
			return 2, true
		}
		return 0, false
	}
}

func printUsage(w io.Writer) {
	banner.PrintLogo(w)
	var cyan, magenta, yellow, dim, bold, reset string
	if banner.IsTTY(w) {
		cyan = "\x1b[36m"
		magenta = "\x1b[35m"
		yellow = "\x1b[33m"
		dim = "\x1b[2m"
		bold = "\x1b[1m"
		reset = "\x1b[0m"
	}
	// Version line, just under the logo. Dim on TTY so it doesn't compete
	// with the banner. Suppressed entirely when ldflags weren't set (so
	// `go run ./cmd/txco --help` from a dev tree stays uncluttered).
	if line := versionLine(); line != "" {
		fmt.Fprintf(w, "                     %s%s%s\n", dim, line, reset)
	}
	// Token painters — concatenate ANSI around a string. No-ops when not TTY.
	heading := func(s string) string { return bold + cyan + s + reset }
	category := func(s string) string { return bold + magenta + s + reset } // command group labels
	cmdName := func(s string) string { return bold + s + reset }
	example := func(s string) string { return cyan + s + reset }
	comment := func(s string) string { return dim + s + reset }
	muted := func(s string) string { return dim + s + reset } // command/flag descriptions
	hint := func(s string) string { return yellow + s + reset }

	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }

	p("\n\n%s\n", heading("Usage:"))
	p("  txco [flags]\n")
	p("  txco <command> [flags]\n\n")

	// "New here?" callout — a mini banner echoing the logo's boxes, nudging
	// first-timers toward the guided demo. Width is derived from the text so
	// the borders always line up (the colored command is zero-width ANSI).
	intro, demoCmd := "New here? Try: ", "txco demo"
	bar := strings.Repeat("─", len(intro)+len(demoCmd)+4)
	p("  %s┌%s┐%s\n", yellow, bar, reset)
	p("  %s│%s  %s%s  %s│%s\n", yellow, reset, intro, cmdName(demoCmd), yellow, reset)
	p("  %s└%s┘%s\n\n", yellow, bar, reset)

	p("%s\n", heading("Examples:"))
	p("  %s\n", comment("# Open the admin web interface"))
	p("  %s\n\n", example("txco ui"))
	p("  %s\n", comment("# Scaffold a new stack and start the chassis"))
	p("  %s\n", example("txco init my-stack"))
	p("  %s\n\n", example("txco serve"))
	p("  %s\n", comment("# Push local rules to a running chassis"))
	p("  %s\n\n", example("txco push"))
	p("  %s\n", comment("# First-time auth setup (keygen + enroll)"))
	p("  %s\n\n", example("txco auth bootstrap-local --secret <s>"))

	// Command table. Label + description live together in one row, so a
	// description can never drift away from its command (the bug the old
	// split format-string/positional-args layout invited). Descriptions may
	// embed a colored hint; padCmd measures the label's visible width, so
	// trailing ANSI in the description doesn't disturb the column.
	p("%s\n", heading("Available commands:"))
	type row struct{ label, desc string }
	type group struct {
		name string
		rows []row
	}
	groups := []group{
		{"Run", []row{
			{"serve", muted("Run the chassis server")},
			{"dev", muted("Spawn apps + chassis, watch for changes (add --apply for startup push)")},
			{"demo", muted("Boot a chassis and open the txcl demo in your browser")},
		}},
		{"Author & deploy", []row{
			{"init <stack> [<dir>]", muted("Scaffold a local OPS/<stack>/.../ tree")},
			{"edit <stack> <path>", muted("Open $EDITOR on one file from a draft and PATCH the result back")},
			{"draft <stack> [<dir>]", muted("Create a draft version of one stack (stage; no activate)")},
			{"apply [<dir>]", muted("Deploy the whole OPS/ tree (all stacks; create + activate a version)")},
			{"push <stack> [<dir>]", muted("Deploy one stack — create + activate (inverse of pull)")},
			{"pull <stack> [<dir>]", muted("Materialise a stack's active version into local OPS/")},
			{"activate <stack>", muted("Flip a stack's active version (defaults to most recent draft)")},
			{"diff [<dir>]", muted("Compare local OPS/ tree against a chassis admin endpoint")},
			{"status [<dir>]", muted("Per-stack version drift between local and chassis (exit 1 on divergence)")},
			{"versions <stack>", muted("List versions for a stack with active marker")},
		}},
		{"Packages & ops", []row{
			{"op <command>", muted("Author + build sandboxed op:// nano-ops (init/build/run/test)")},
			{"install <source> --as <stack>", muted("Install a package into OPS/ (sales@v3, oci:, dir:, github:), then apply")},
			{"package <command>", muted("Author + manage packages (init/validate/publish · list/upgrade/remove)")},
		}},
		{"Identity & access", []row{
			{"auth <command>", muted("Manage signing keys for the admin API")},
			{"ui", muted("Open the chassis admin UI in your browser, signed in via your profile")},
			{"login", muted("Sign in to the thanks-computer cloud")},
			{"logout", muted("Sign out of the thanks-computer cloud")},
			{"config <command>", muted("Alias namespace for profile / logout (gcloud/stripe-style)")},
		}},
		{"Diagnose & connect", []row{
			{"trace [<rid>]", muted("Render the execution trace for a request (use ") + hint("`txco trace last`") + muted(" for the most recent)")},
			{"doctor", muted("Diagnose local setup + chassis reachability (auth/keys/version)")},
			{"mcp <command>", muted("Talk to MCP-over-HTTP servers (use ") + hint("`txco mcp doctor`") + muted(" for discovery)")},
			{"room [--room N] <msg>", muted("Send a message into a room (also installed as ") + hint("thanks") + muted(")")},
		}},
		{"CLI", []row{
			{"version", muted("Print version info")},
			{"update check", muted("Check for a newer txco CLI release on GitHub")},
			{"upgrade", muted("Upgrade the txco CLI binary (self-update, or brew/source guidance)")},
			{"completion <shell>", muted("Emit a shell completion script (use ") + hint("`txco completion bash|zsh|fish`") + muted(" for install steps)")},
		}},
	}
	// Each category is a magenta header; its commands indent one level under it.
	for _, g := range groups {
		p("\n  %s\n", category(g.name))
		for _, r := range g.rows {
			p("    %s   %s\n", padCmd(cmdName(r.label)), r.desc)
		}
	}

	p("\n%s\n", heading("Common flags:"))
	flags := []row{
		{"--profile NAME", muted("Signing profile (TXCO_PROFILE, then your active profile, then 'local')")},
		{"--json", muted("Emit output as JSON")},
	}
	for _, f := range flags {
		p("  %s   %s\n", padCmd(cmdName(f.label)), f.desc)
	}

	p("\nUse %s for per-command flags.\n", hint("`txco <command> --help`"))
}

// padCmd left-aligns a command label to a fixed visible width so the
// description column lines up across entries. ANSI escape codes are
// invisible but count as bytes — so we pad based on the visible
// substring (sans codes).
func padCmd(s string) string {
	const width = 23
	visible := stripANSI(s)
	if len(visible) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(visible))
}

// runVersion emits the build identity as JSON (pretty-indented) on stdout.
// Reachable via `txco version`, `txco --version`, and `txco -v`. JSON keeps
// the output machine-parseable from a release-automation script while
// still being readable at a terminal.
func runVersion(w io.Writer) int {
	version, commit := buildVersionCommit()
	info := struct {
		Version        string `json:"version"`
		Commit         string `json:"commit"`
		Chassis        string `json:"chassis,omitempty"`
		BuildTimestamp string `json:"build_timestamp"`
		InstallMethod  string `json:"install_method"`
		GoVersion      string `json:"go_version"`
		OS             string `json:"os"`
		Arch           string `json:"arch"`
	}{
		Version:        version,
		Commit:         commit,
		Chassis:        Build.Chassis,
		BuildTimestamp: Build.BuildTimestamp,
		InstallMethod:  Build.InstallMethod,
		GoVersion:      runtime.Version(),
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(info); err != nil {
		fmt.Fprintf(os.Stderr, "txco: version: %v\n", err)
		return 1
	}
	return 0
}

// buildVersionCommit returns the build's version + commit, falling back to
// the Go-embedded module build info when ldflags weren't set (e.g. a
// `go install`/`go run` build, or a wrapper image that forgot to stamp).
// This makes `txco version` self-report rather than echo a stale literal —
// mirrors chassis/snapshot/snapshot.go:sourceChassisVersion.
func buildVersionCommit() (version, commit string) {
	version, commit = Build.Version, Build.CommitId
	if version != "" && commit != "" && commit != "dev" {
		return version, commit
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit
	}
	if version == "" && bi.Main.Version != "" {
		version = bi.Main.Version
	}
	if commit == "" || commit == "dev" {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				commit = s.Value
				break
			}
		}
	}
	return version, commit
}

// versionLine renders the one-line version banner shown under the logo on
// the help screen. Returns "" when no version info has been wired
// (`Build.Version == ""`) so dev runs through `go run` stay quiet. Commit
// is shortened to 7 chars (git's default short form). Build timestamp is
// truncated to the date for compactness.
func versionLine() string {
	v := strings.TrimPrefix(Build.Version, "v")
	if v == "" {
		return ""
	}
	parts := []string{"v" + v}
	if c := Build.CommitId; c != "" && c != "dev" {
		if len(c) > 7 {
			c = c[:7]
		}
		parts = append(parts, "commit "+c)
	}
	if ts := Build.BuildTimestamp; ts != "" {
		// Trim to YYYY-MM-DD if it looks like an RFC3339 timestamp.
		if len(ts) >= 10 && ts[4] == '-' && ts[7] == '-' {
			ts = ts[:10]
		}
		parts = append(parts, "built "+ts)
	}
	return strings.Join(parts, " · ")
}

// stripANSI removes CSI escape sequences from s. Cheap enough for the
// few short strings in the help screen; not a general-purpose ANSI
// stripper.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			// skip until letter (the CSI terminator)
			j := i + 2
			for j < len(s) && !(s[j] >= '@' && s[j] <= '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// resolveDir returns the absolute path of dir (defaulting to cwd when empty).
func resolveDir(dir string) (string, error) {
	if dir == "" {
		dir = "."
	}
	return filepath.Abs(dir)
}

// findWorkspaceRoot returns the nearest directory at or above start that
// contains an `OPS/` subdirectory, or "" if none up to the filesystem
// root. Lets `txco apply` (and friends) be run from anywhere inside the
// tree — like git finding its repo root — instead of only from the dir
// that literally contains OPS/. An explicit <dir> arg still wins (the
// caller passes that straight to resolveDir and only falls back here).
func findWorkspaceRoot(start string) string {
	d := start
	for {
		if fi, err := os.Stat(filepath.Join(d, "OPS")); err == nil && fi.IsDir() {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d { // hit filesystem root
			return ""
		}
		d = parent
	}
}
