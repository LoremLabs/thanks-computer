package auth

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// `txco auth tenant connections <sub>`: the operator's read of a tenant's
// connections (chassis/connection) — who is connected where — and a
// revoke. Metadata only: nothing here shows a token, and no verb could.
// Connecting is the stack's job (txco://connection/start, on a person's
// sign-in), not the CLI's.

func runTenantConnections(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTenantConnectionsUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list", "ls":
		return runConnectionsList(args[1:], stdout, stderr)
	case "revoke", "rm":
		return runConnectionsRevoke(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printTenantConnectionsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth tenant connections: unknown subcommand %q\n\n", args[0])
		printTenantConnectionsUsage(stderr)
		return 2
	}
}

func printTenantConnectionsUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  txco auth tenant connections list   --principal P | --provider N  [--tenant SLUG]
  txco auth tenant connections revoke --principal P --provider N    [--tenant SLUG] [-y]

A connection is a principal's identity at an outside service — a person's
or a service's GitHub account, say — held and renewed by the chassis
(PROVIDERS/<name>.yaml declares the service; a stack connects a principal
with txco://connection/start on the person's sign-in). This lists who is
connected where, and removes one. Nothing here shows a token.
`)
}

func connectionsTarget(what string, url, profile, targetSel, tenant *string, stderr io.Writer) (client.Target, bool) {
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections %s: %v", what, err)
		return client.Target{}, false
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections %s: %v", what, err)
		return client.Target{}, false
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant connections "+what+": no signing key configured")
		return client.Target{}, false
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	return target, true
}

func runConnectionsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant connections list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	principal := fs.String("principal", "", "a principal's connections (service:pony-a, user:usr_…)")
	provider := fs.String("provider", "", "a provider's connections (github)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*principal == "") == (*provider == "") {
		PrintCLIError(stderr, "auth tenant connections list: pass --principal or --provider, one of them")
		return 2
	}
	target, ok := connectionsTarget("list", url, profile, targetSel, tenant, stderr)
	if !ok {
		return 1
	}
	rows, err := client.New(target).ListConnections(context.Background(), *principal, *provider)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections list: %v", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "(no connections in tenant %q)\n", target.Tenant)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PRINCIPAL\tPROVIDER\tKIND\tACCOUNT\tSTATUS\tEXPIRES_AT\tSCOPES\tCREATED_BY")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Principal, r.Provider, r.Kind, dashIfEmpty(r.Account), r.Status, dashIfEmpty(r.ExpiresAt),
			dashIfEmpty(strings.Join(r.Scopes, ",")), r.CreatedBy)
	}
	_ = tw.Flush()
	return 0
}

func runConnectionsRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant connections revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	principal := fs.String("principal", "", "the principal (service:pony-a, user:usr_…)")
	provider := fs.String("provider", "", "the provider (github)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *principal == "" || *provider == "" {
		PrintCLIError(stderr, "auth tenant connections revoke: --principal and --provider are required")
		return 2
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections revoke: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections revoke: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant connections revoke: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	if err := ConfirmTargetStd(resolvedProfile, target.Addr, *yes, false, stderr); err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections revoke: %v", err)
		return 1
	}
	if err := client.New(target).RevokeConnection(context.Background(), *principal, *provider); err != nil {
		PrintCLIErrorf(stderr, "auth tenant connections revoke: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked: %s at %s (tenant %q)\n", *principal, *provider, target.Tenant)
	return 0
}
