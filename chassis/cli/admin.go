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
	case "tenant":
		return runAdminTenant(args[1:], stdout, stderr)
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
  txco admin resync --tenant <slug>            Re-emit a tenant's control-plane state to the fleet
  txco admin tenant suspend <slug> [--status]  Deny a tenant's requests (default 402) until resumed
  txco admin tenant resume <slug>              Restore a suspended tenant

Operator-facing chassis maintenance. `+"`resync`"+` re-broadcasts one tenant's
current rows (tenant + hostnames + active stacks) as fleet-sync events so a
replica that missed an event converges. `+"`tenant suspend/resume`"+` flips a
tenant's request admission (the 402/403 gate), live on the next request.
All require a super_admin signing profile.

To open the admin web UI in your browser, use `+"`txco auth login`"+` (it signs
you in and opens the console).
`)
}

// runAdminTenant dispatches `txco admin tenant <suspend|resume> <slug>`.
func runAdminTenant(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAdminTenantUsage(stderr)
		return 2
	}
	switch args[0] {
	case "suspend":
		return runAdminTenantSuspend(args[1:], stdout, stderr)
	case "resume":
		return runAdminTenantResume(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printAdminTenantUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "admin tenant: unknown subcommand %q\n\n", args[0])
		printAdminTenantUsage(stderr)
		return 2
	}
}

func printAdminTenantUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco admin tenant suspend <slug> [--status 402] [--reason payment_required]
  txco admin tenant resume  <slug>

Suspend or resume a tenant's request admission. A suspended tenant's requests
are denied (HTTP --status, default 402) before its stack runs; resume restores
normal serving. Live on the next request (the chassis reloads its in-memory
state); on a fleet deployment the change is also queued to replicas. Requires a
super_admin signing profile.
`)
}

// runAdminTenantSuspend: `txco admin tenant suspend <slug> [--status N --reason R]`.
func runAdminTenantSuspend(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin tenant suspend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	status := fs.Int("status", 402, "HTTP status a suspended tenant's requests return (e.g. 402 or 403)")
	reason := fs.String("reason", "payment_required", "machine reason surfaced as the x-txc-deny-reason header")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides the active profile's chassis URL)")
	target := fs.String("target", "", "workspace target name")
	user := fs.String("user", "", "basic-auth user")
	pass := fs.String("pass", "", "basic-auth password")
	profile := fs.String("profile", "", "signing profile (defaults to the active profile)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco admin tenant suspend <slug> [flags]

Mark a tenant suspended so its requests are denied (HTTP --status, default 402)
before its stack runs. Live on the next request. Needs a super_admin profile.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		auth.PrintCLIError(stderr, "admin tenant suspend: <slug> is required")
		return 2
	}
	slug := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil { // allow flags after the slug
		return 2
	}

	clientTarget := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	st, err := client.New(clientTarget).SuspendTenant(context.Background(), slug,
		client.SuspendTenantRequest{DenyStatus: *status, DenyReason: *reason})
	if err != nil {
		auth.PrintCLIError(stderr, requestErrorMessage("admin tenant suspend", clientTarget, *profile, err))
		return 1
	}
	fmt.Fprintf(stdout, "Suspended tenant %q — requests now return %d (%s).\n", st.Slug, st.DenyStatus, st.DenyReason)
	fmt.Fprintf(stdout, "Resume with: txco admin tenant resume %s\n", st.Slug)
	return 0
}

// runAdminTenantResume: `txco admin tenant resume <slug>`.
func runAdminTenantResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin tenant resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "", "chassis admin endpoint (overrides the active profile's chassis URL)")
	target := fs.String("target", "", "workspace target name")
	user := fs.String("user", "", "basic-auth user")
	pass := fs.String("pass", "", "basic-auth password")
	profile := fs.String("profile", "", "signing profile (defaults to the active profile)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco admin tenant resume <slug> [flags]

Clear a tenant's suspension so its requests are admitted again. Live on the next
request. Needs a super_admin profile.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		auth.PrintCLIError(stderr, "admin tenant resume: <slug> is required")
		return 2
	}
	slug := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	clientTarget := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	st, err := client.New(clientTarget).ResumeTenant(context.Background(), slug)
	if err != nil {
		auth.PrintCLIError(stderr, requestErrorMessage("admin tenant resume", clientTarget, *profile, err))
		return 1
	}
	fmt.Fprintf(stdout, "Resumed tenant %q — requests are admitted again.\n", st.Slug)
	return 0
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
		auth.PrintCLIError(stderr, requestErrorMessage("admin resync", clientTarget, *profile, err))
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
