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
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	opcli "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
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
		return runApply(rest, stdout, stderr), true
	case "diff":
		return runDiff(rest, stdout, stderr), true
	case "status":
		return runStatus(rest, stdout, stderr), true
	case "pull":
		return runPull(rest, stdout, stderr), true
	case "push":
		return runPush(rest, stdout, stderr), true
	case "activate":
		return runActivate(rest, stdout, stderr), true
	case "versions":
		return runVersions(rest, stdout, stderr), true
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
	case "op":
		return opcli.Dispatch(rest, stdout, stderr), true
	case "install":
		return runInstall(rest, stdout, stderr), true
	case "inspect":
		return runInspect(rest, stdout, stderr), true
	case "package":
		return runPackage(rest, stdout, stderr), true
	case "mcp":
		return runMcp(rest, stdout, stderr), true
	case "config":
		return runConfig(rest, stdout, stderr), true
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0, true
	case "version", "--version", "-v":
		return runVersion(stdout), true
	default:
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
	var (
		cyan, yellow, dim, bold, reset string
	)
	if banner.IsTTY(w) {
		cyan = "\x1b[36m"
		yellow = "\x1b[33m"
		dim = "\x1b[2m"
		bold = "\x1b[1m"
		reset = "\x1b[0m"
	}
	// Version line, just under the logo. Dim on TTY so it doesn't compete
	// with the banner. Suppressed entirely when ldflags weren't set (so
	// `go run ./cmd/txco --help` from a dev tree stays uncluttered).
	if line := versionLine(); line != "" {
		fmt.Fprintf(w, "%s%s%s\n", dim, line, reset)
	}
	// Helpers — concatenate ANSI around tokens. No-ops when not TTY.
	heading := func(s string) string { return bold + cyan + s + reset }
	cmd := func(s string) string { return bold + s + reset }
	example := func(s string) string { return cyan + s + reset }
	comment := func(s string) string { return dim + s + reset }
	hint := func(s string) string { return yellow + s + reset }

	fmt.Fprintf(w, `
%s
  txco [flags]
  txco <command> [flags]

The thanks-computer chassis: event router + rule authoring CLI.

%s
  %s
  %s
  %s

  %s
  %s

  %s
  %s

%s
  %s   Run the chassis server
  %s   Scaffold a local OPS/<stack>/.../ tree
  %s   Push local OPS/ tree to a chassis admin endpoint (push + activate)
  %s   Compare local OPS/ tree against a chassis admin endpoint
  %s   Per-stack version drift between local and chassis (exit 1 on divergence)
  %s   Materialise a stack's active version into local OPS/
  %s   Create a draft version from local OPS/<stack>/ (add --activate to deploy)
  %s   Flip a stack's active version (defaults to most recent draft)
  %s   List versions for a stack with active marker
  %s   Open $EDITOR on one file from a draft and PATCH the result back
  %s   Spawn apps + chassis, watch for changes (add --apply for startup push)
  %s   Boot a chassis and open the txcl demo in your browser
  %s   Author + build sandboxed op:// nano-ops (init/build/run/test)
  %s   Install a package into OPS/, then run apply (dir:/github:)
  %s   Inspect a package without installing (dir:/github:)
  %s   Author + validate TxCo packages (init/validate)
  %s   Render the execution trace for a request (use %s for the most recent)
  %s   Manage signing keys for the admin API
  %s   Talk to MCP-over-HTTP servers (use %s for discovery)
  %s   Alias namespace for profile / logout (gcloud/stripe-style)
  %s   Print version info as JSON

%s
  %s   Target name from txco.yaml (default: 'dev')
  %s   Admin endpoint (overrides target's chassis URL)
  %s   Basic auth user
  %s   Basic auth password

Use %s for per-command flags.
`,
		heading("Usage:"),
		heading("Examples:"),
		comment("# Scaffold a new stack and start the chassis"),
		example("txco init my-stack"),
		example("txco serve"),
		comment("# Push local rules to a running chassis"),
		example("txco apply"),
		comment("# First-time auth setup (keygen + enroll)"),
		example("txco auth bootstrap-local --secret <s>"),
		heading("Available commands:"),
		padCmd(cmd("serve")),
		padCmd(cmd("init")+" <stack> [<dir>]"),
		padCmd(cmd("apply")+" [<dir>]"),
		padCmd(cmd("diff")+"  [<dir>]"),
		padCmd(cmd("status")+" [<dir>]"),
		padCmd(cmd("pull")+" <stack> [<dir>]"),
		padCmd(cmd("push")+" <stack> [<dir>]"),
		padCmd(cmd("activate")+" <stack>"),
		padCmd(cmd("versions")+" <stack>"),
		padCmd(cmd("edit")+" <stack> <path>"),
		padCmd(cmd("dev")),
		padCmd(cmd("demo")),
		padCmd(cmd("op")+" <command>"),
		padCmd(cmd("install")+" <source> --as <stack>"),
		padCmd(cmd("inspect")+" <source>"),
		padCmd(cmd("package")+" <command>"),
		padCmd(cmd("trace")+" [<rid>]"), hint("`txco trace last`"),
		padCmd(cmd("auth")+" <command>"),
		padCmd(cmd("mcp")+" <command>"), hint("`txco mcp doctor`"),
		padCmd(cmd("config")+" <command>"),
		padCmd(cmd("version")),
		heading("Common flags for apply/diff/status/dev/trace:"),
		padCmd(cmd("--target NAME")),
		padCmd(cmd("--addr URL")),
		padCmd(cmd("--user USER")),
		padCmd(cmd("--pass PASS")),
		hint("`txco <command> --help`"),
	)
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
	info := struct {
		Version        string `json:"version"`
		Commit         string `json:"commit"`
		BuildTimestamp string `json:"build_timestamp"`
		GoVersion      string `json:"go_version"`
		OS             string `json:"os"`
		Arch           string `json:"arch"`
	}{
		Version:        Build.Version,
		Commit:         Build.CommitId,
		BuildTimestamp: Build.BuildTimestamp,
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
