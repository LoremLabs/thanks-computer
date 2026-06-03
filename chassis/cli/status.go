package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runStatus prints just the per-stack version drift report (the same
// "stacks:" section `txco diff` emits before its file changes) and
// nothing else. Useful as a startup sanity check or a shell-scriptable
// "am I in sync?" probe — exit code is 0 when all stacks are aligned
// and 1 when any are divergent or unknown.
func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("status", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml")
	addr := fs.String("addr", "", "chassis admin endpoint")
	user := fs.String("user", "", "basic auth user")
	pass := fs.String("pass", "", "basic auth password")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of the table")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco status [flags] [<dir>]

Print per-stack version drift between the local workspace and the
chassis. Shows whether each stack is in-sync, ahead/behind, or
untracked, plus each stack's reachable URL. Exit code is 0 when all
stacks are in sync, 1 otherwise (in both table and --json modes).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := resolveDir(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "status: resolve dir: %v\n", err)
		return 1
	}

	// Walk local OPS just enough to learn the stack set. We don't need
	// the full bundle.Op records — buildDrifts re-reads files from disk
	// when it wants the manifest hash, so a stack-name walk is enough.
	localOps, err := bundle.Walk(dir)
	if err != nil {
		fmt.Fprintf(stderr, "status: walk %s: %v\n", dir, err)
		return 1
	}

	clientTarget := resolveTarget(dir, *target, *addr, *user, *pass, *profile)
	clientTarget.Tenant = resolveTenant(*tenant, *profile)
	c := client.New(clientTarget)

	// Pull remote stack names from /stacks rather than /ops so we
	// surface stacks that exist on the chassis but have no local OPS/
	// dir yet. ListOps would miss those when --target's tenant has no
	// rows in the legacy ops table.
	//
	// Bail loudly when the chassis is unreachable — silently dropping
	// the error hides connectivity issues and produces a drift table
	// that looks like "every stack has no remote active version" when
	// the real story is "chassis is down". The user explicitly asked
	// for this signal during the cheap-wins pass.
	remoteStacks, err := c.ListStacks(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "status: cannot reach chassis at %s: %v\n", clientTarget.Addr, err)
		return 1
	}
	var remoteNames []string
	for _, s := range remoteStacks {
		remoteNames = append(remoteNames, s.Name)
	}

	drifts := buildDrifts(context.Background(), c, dir, localOps, remoteNames)
	// Annotate each stack with a reachable URL so the user can click
	// straight through. Best-effort: a chassis without hostname routing
	// just leaves the column (or JSON field) off.
	if len(drifts) > 0 {
		decorateStackURLs(context.Background(), c, drifts)
	}

	if *jsonOut {
		return emitStatusJSON(stdout, stderr, drifts)
	}

	if len(drifts) == 0 {
		fmt.Fprintln(stdout, "no stacks found locally or remotely")
		return 0
	}
	if printDriftTable(stdout, drifts) {
		return 1
	}
	return 0
}

// emitStatusJSON writes the drift list as a JSON array (one object per
// stack) and returns the same exit code as the table form: 0 when every
// stack is in sync, 1 when any is divergent — so `txco status --json`
// stays usable as a scriptable "am I in sync?" probe. An empty workspace
// emits `[]`, not the human "no stacks" line.
func emitStatusJSON(stdout, stderr io.Writer, drifts []stackDrift) int {
	any := false
	for _, d := range drifts {
		if d.Divergent {
			any = true
			break
		}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(driftsToJSON(drifts)); err != nil {
		fmt.Fprintf(stderr, "status: encode json: %v\n", err)
		return 1
	}
	if any {
		return 1
	}
	return 0
}
