package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/update"
)

// runCLIUpdate routes `txco update <subcommand>` — currently just `check`.
// Named runCLIUpdate (not runUpdate) to avoid colliding with the existing
// runUpgrade for `txco package upgrade`; this commands the CLI binary, not
// installed packages.
func runCLIUpdate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUpdateUsage(stdout)
		return 0
	}
	switch args[0] {
	case "check":
		return runUpdateCheck(stdout, stderr)
	case "help", "-h", "--help":
		printUpdateUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "update: unknown subcommand %q\n\n", args[0])
		printUpdateUsage(stderr)
		return 2
	}
}

func runUpdateCheck(stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	res, err := update.Check(ctx, Build.Version, userAgent())
	if err != nil {
		fmt.Fprintf(stderr, "txco: update check: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Current version: %s\n", displayVersion(res.Current))
	fmt.Fprintf(stdout, "Latest version:  v%s\n\n", res.Latest)
	switch {
	case !res.Comparable:
		fmt.Fprintf(stdout, "Can't compare (this build has no release version).\n")
	case res.Available:
		fmt.Fprintf(stdout, "Update available. Run `txco upgrade`.\n")
	default:
		fmt.Fprintf(stdout, "You're up to date.\n")
	}

	// Best-effort: also report the chassis we'd talk to (its build identity —
	// the "what's deployed?" debug surface) and warn (warn-only) if this CLI
	// is below the server's advertised minimum. Silently skipped when no
	// chassis is reachable (e.g. not logged in / no local server).
	reportServerPolicy(stdout)
	return 0
}

// reportServerPolicy fetches the resolved chassis's /healthz JSON and prints
// its build identity plus any client-version warning. Entirely best-effort:
// any error (no target, unreachable, non-JSON) is swallowed.
func reportServerPolicy(stdout io.Writer) {
	target := resolveTarget("", "", "", "", "", "")
	if target.Addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := update.FetchServerInfo(ctx, target.Addr, userAgent())
	if err != nil {
		return
	}
	fmt.Fprintf(stdout, "\nServer %s is running %s\n", target.Addr, serverBuildSummary(info))
	if notice := update.OutdatedNotice(Build.Version, info.Client); notice != "" {
		fmt.Fprintf(stdout, "%s\n", notice)
	}
}

// serverBuildSummary renders a chassis's self-reported build for humans,
// omitting parts the server didn't stamp.
func serverBuildSummary(info update.ServerInfo) string {
	parts := []string{}
	if info.Version != "" {
		parts = append(parts, "v"+strings.TrimPrefix(info.Version, "v"))
	}
	if info.Commit != "" {
		c := info.Commit
		if len(c) > 7 {
			c = c[:7]
		}
		parts = append(parts, "commit "+c)
	}
	if info.Chassis != "" {
		parts = append(parts, "chassis "+info.Chassis)
	}
	if len(parts) == 0 {
		return "(version unknown)"
	}
	return strings.Join(parts, " · ")
}

// runCLIUpgrade routes `txco upgrade`: package-managed and source builds get
// delegation guidance; self-managed (manual/curl) installs self-update.
func runCLIUpgrade(args []string, stdout, stderr io.Writer) int {
	m := update.ResolveCurrent(Build.InstallMethod)
	if !m.SelfUpdate {
		fmt.Fprintln(stdout, update.UpgradeGuidance(m))
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := update.Check(ctx, Build.Version, userAgent())
	if err != nil {
		fmt.Fprintf(stderr, "txco: upgrade: %v\n", err)
		return 1
	}
	if res.Comparable && !res.Available {
		fmt.Fprintf(stdout, "Already up to date (v%s).\n", res.Current)
		return 0
	}

	fmt.Fprintf(stdout, "Downloading txco v%s…\n", res.Latest)
	newVer, err := update.SelfUpdate(ctx, userAgent())
	if err != nil {
		fmt.Fprintf(stderr, "txco: upgrade: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Updated %s → v%s.\n", displayVersion(res.Current), newVer)
	return 0
}

// userAgent identifies the CLI to GitHub with its version (replaces the
// legacy hardcoded "txco-cli/1").
func userAgent() string {
	v := Build.Version
	if v == "" {
		v = "dev"
	}
	return "txco-cli/" + v
}

func displayVersion(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return "v" + v
}

func printUpdateUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: txco update <command>

Check whether a newer txco CLI release is available (GitHub Releases).

Commands:
  check   Compare the running version against the latest release

To upgrade, run `+"`txco upgrade`"+`: self-managed installs self-update;
Homebrew installs are told to run `+"`brew upgrade txco`"+`.
`)
}
