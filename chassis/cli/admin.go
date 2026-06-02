package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runAdmin dispatches `txco admin <subcommand>` — operator-facing chassis
// maintenance. Distinct from `txco auth` (identity) and `txco cloud` (account).
func runAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAdminUsage(stdout)
		return 0
	}
	switch args[0] {
	case "resync":
		return runAdminResync(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printAdminUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "admin: unknown subcommand %q\n\n", args[0])
		printAdminUsage(stderr)
		return 2
	}
}

func printAdminUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco admin resync --tenant <slug>   Re-emit a tenant's control-plane state to the fleet

Operator-facing chassis maintenance. `+"`resync`"+` re-broadcasts one tenant's
current rows (tenant + hostnames + active stacks) as fleet-sync events so a
replica that missed an event converges. Non-destructive (idempotent upserts).
Requires a super_admin signing profile.
`)
}

// runAdminResync re-emits ONE tenant's control-plane state as fresh fleet-sync
// events. Requires --tenant; there is no fleet-wide fan-out (a full rebuild is
// a snapshot bootstrap, not a resync).
func runAdminResync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin resync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tenant := fs.String("tenant", "", "tenant slug to resync (required)")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides the active profile's chassis URL)")
	target := fs.String("target", "", "workspace target name")
	user := fs.String("user", "", "basic-auth user")
	pass := fs.String("pass", "", "basic-auth password")
	profile := fs.String("profile", "", "signing profile (defaults to the active profile)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco admin resync --tenant <slug> [flags]

Re-emit a tenant's current control-plane state (its row + hostnames + active
stack versions) as fresh fleet-sync events, so any replica that missed an event
converges. Non-destructive: only upserts are emitted, never deletes, so it's
safe to re-run.

Needs a super_admin signing profile (it's a chassis-wide control operation).
A full fleet rebuild is a snapshot bootstrap, not a resync.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *tenant == "" {
		auth.PrintCLIError(stderr, "admin resync: --tenant <slug> is required")
		return 2
	}

	clientTarget := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	c := client.New(clientTarget)
	resp, err := c.FleetResync(context.Background(), client.FleetResyncRequest{TenantSlug: *tenant})
	if err != nil {
		auth.PrintCLIErrorf(stderr, "admin resync: %v", err)
		return 1
	}
	if !resp.FleetEnabled {
		fmt.Fprintf(stdout, "Fleet sync is disabled on this chassis (--feed-sink=nop); nothing to resync.\n")
		return 0
	}
	fmt.Fprintf(stdout, "Resynced tenant %q — queued %d tenant, %d hostname, %d stack event(s).\n",
		resp.TenantSlug, resp.Events.TenantCreated, resp.Events.HostnameBound, resp.Events.StackActivated)
	fmt.Fprintf(stdout, "Replicas converge on their next poll.\n")
	return 0
}
