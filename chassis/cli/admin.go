package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/clientcmd"
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
		// An overlay may extend `admin` with its own operator subcommands
		// (chassis/clientcmd admin registry) — e.g. the cloud overlay's
		// `admin credits`. Open core registers none, so a self-hosted chassis
		// falls straight through to the unknown-subcommand error.
		if h, ok := clientcmd.LookupAdmin(args[0]); ok {
			return runClientCmd(h, args[1:], stdout, stderr)
		}
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
	case "limits":
		return runAdminTenantLimits(args[1:], stdout, stderr)
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
  txco admin tenant limits  <slug> [--rate 50/s] [--burst N] [--concurrency N]

Suspend/resume a tenant's request admission (the 402/403 gate), or set its
node-local rate-limit / concurrency caps (429). A suspended tenant's requests
are denied before its stack runs; limits are enforced per chassis. Live on the
next request (the chassis reloads its in-memory state); on a fleet deployment
the change is also queued to replicas. Requires a super_admin signing profile.
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

// runAdminTenantLimits: `txco admin tenant limits <slug> [--rate N/{s,m,h}] [--burst N] [--concurrency N]`.
// Patch semantics: only the flags you pass are changed.
func runAdminTenantLimits(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin tenant limits", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rateStr := fs.String("rate", "", "per-tenant rate limit, e.g. 50/s, 50/m, 100/h (0 disables); bare number = /s")
	burst := fs.Int("burst", 0, "token-bucket burst size; defaults to ceil(2×rate)")
	concurrency := fs.Int("concurrency", 0, "max simultaneous in-flight requests (0 = unlimited)")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides the active profile's chassis URL)")
	target := fs.String("target", "", "workspace target name")
	user := fs.String("user", "", "basic-auth user")
	pass := fs.String("pass", "", "basic-auth password")
	profile := fs.String("profile", "", "signing profile (defaults to the active profile)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco admin tenant limits <slug> [flags]

Set a tenant's node-local rate-limit and/or concurrency cap (operational
protection, enforced per chassis — not a billing meter). Only the flags you
pass are changed. Live on the next request. Needs a super_admin profile.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		auth.PrintCLIError(stderr, "admin tenant limits: <slug> is required")
		return 2
	}
	slug := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil { // allow flags after the slug
		return 2
	}

	// Patch semantics — only send the flags the operator actually set.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var req client.SetTenantLimitsRequest
	if set["rate"] {
		rps, err := parseRate(*rateStr)
		if err != nil {
			auth.PrintCLIErrorf(stderr, "admin tenant limits: --rate %q: %v", *rateStr, err)
			return 2
		}
		req.RPS = &rps
	}
	if set["burst"] {
		req.Burst = burst
	}
	if set["concurrency"] {
		req.Concurrency = concurrency
	}
	if req.RPS == nil && req.Burst == nil && req.Concurrency == nil {
		auth.PrintCLIError(stderr, "admin tenant limits: set at least one of --rate / --burst / --concurrency")
		return 2
	}

	clientTarget := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	st, err := client.New(clientTarget).SetTenantLimits(context.Background(), slug, req)
	if err != nil {
		auth.PrintCLIError(stderr, requestErrorMessage("admin tenant limits", clientTarget, *profile, err))
		return 1
	}
	fmt.Fprintf(stdout, "Limits for tenant %q: rate=%s burst=%d concurrency=%d\n",
		st.Slug, formatRate(st.RateLimitRPS), st.RateBurst, st.ConcurrencyLimit)
	return 0
}

// parseRate parses "<number>[/unit]" (unit s|m|h, default s) into a
// per-second rate. 0 means unlimited.
func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(s)
	num, per := s, 1.0 // per = seconds per unit
	if i := strings.IndexByte(s, '/'); i >= 0 {
		num = strings.TrimSpace(s[:i])
		switch strings.ToLower(strings.TrimSpace(s[i+1:])) {
		case "s", "sec", "second":
			per = 1
		case "m", "min", "minute":
			per = 60
		case "h", "hr", "hour":
			per = 3600
		default:
			return 0, fmt.Errorf("unknown unit (use s, m, or h)")
		}
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("not a number: %q", num)
	}
	if n < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return n / per, nil
}

// formatRate renders a per-second rate back in a friendly unit.
func formatRate(rps float64) string {
	switch {
	case rps <= 0:
		return "unlimited"
	case rps >= 1:
		return strconv.FormatFloat(rps, 'g', -1, 64) + "/s"
	case rps*60 >= 1:
		return strconv.FormatFloat(rps*60, 'g', -1, 64) + "/m"
	default:
		return strconv.FormatFloat(rps*3600, 'g', -1, 64) + "/h"
	}
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
	fmt.Fprintf(stdout, "Resynced tenant %q — queued %d tenant, %d hostname, %d dns-zone, %d stack event(s).\n",
		resp.TenantSlug, resp.Events.TenantCreated, resp.Events.HostnameBound,
		resp.Events.DNSZoneUpserted, resp.Events.StackActivated)
	fmt.Fprintf(stdout, "Replicas converge on their next poll.\n")
	return 0
}
