package auth

// `txco allowance` — a tenant's own fuel budgets (chassis/allowance) over the
// signed admin API (/v1/tenants/{t}/allowances), so it works against a remote
// chassis exactly like `txco kv list`. A stack puts a request in an allowance
// with txco://allowance/enter; this is where the tenant sets and reads them.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// RunAllowance dispatches `txco allowance <sub>` (top-level, wired in cli.go).
func RunAllowance(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAllowanceUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list", "ls":
		return runAllowance("list", args[1:], 0, stdout, stderr)
	case "get", "show":
		return runAllowance("get", args[1:], 1, stdout, stderr)
	case "set":
		return runAllowance("set", args[1:], 1, stdout, stderr)
	case "delete", "rm":
		return runAllowance("delete", args[1:], 1, stdout, stderr)
	case "help", "-h", "--help":
		printAllowanceUsage(stdout)
		return 0
	default:
		PrintCLIErrorf(stderr, "allowance: unknown subcommand %q", args[0])
		printAllowanceUsage(stderr)
		return 2
	}
}

func printAllowanceUsage(w io.Writer) {
	fmt.Fprint(w, `txco allowance — a tenant's own fuel budgets

Usage:
  txco allowance list                         Every allowance and its current window
  txco allowance get <name>                   One allowance's current window
  txco allowance set <name> --fuel N [--per hour|day|month]
                                              Create or replace (per defaults to day)
  txco allowance delete <name>                Remove the definition

Flags:
  --tenant SLUG    tenant to act on (default: the profile's tenant)
  --profile NAME   signing profile
  --target SEL     chassis to act on: a profile name or a raw admin URL
  --url URL        chassis admin endpoint
  --limit N        list page size (max 200)
  --after CURSOR   list: resume after this name
  --all            list: follow pagination

An allowance is fuel per UTC hour, day or month. A stack puts a request in
one with txco://allowance/enter; when the window's fuel is spent the request
is refused with 429 allowance_exhausted until the window resets. The tenant's
own budget still applies on top. Needs the kv capabilities (tenant owners
have them).
`)
}

func runAllowance(sub string, args []string, wantArgs int, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("allowance "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	fuel := fs.Int64("fuel", 0, "set: fuel per window")
	per := fs.String("per", "", "set: hour, day or month (default day)")
	limit := fs.Int("limit", 0, "list: page size (max 200)")
	after := fs.String("after", "", "list: resume cursor")
	all := fs.Bool("all", false, "list: follow pagination")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < wantArgs {
		PrintCLIErrorf(stderr, "allowance %s: NAME is required", sub)
		printAllowanceUsage(stderr)
		return 2
	}
	name := ""
	if wantArgs > 0 {
		name = fs.Arg(0)
		// Re-parse the flags after NAME: the stdlib flag package stops at
		// the first positional, so `set scout --fuel 500` would otherwise
		// drop --fuel. Same idiom as `auth tenant secrets set`.
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return 2
		}
	}
	// A trailing positional selects the target, as in `txco kv list`.
	if *targetSel == "" {
		*targetSel = trailingPositional(fs)
	}
	if sub == "set" && *fuel <= 0 {
		PrintCLIError(stderr, "allowance set: --fuel N is required (fuel per window, N > 0)")
		return 2
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "allowance %s: %v", sub, err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "allowance %s: %v", sub, err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIErrorf(stderr, "allowance %s: no signing key configured", sub)
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	cli := client.New(target)
	ctx := context.Background()
	switch sub {
	case "get":
		a, err := cli.GetAllowance(ctx, name)
		if err != nil {
			PrintCLIErrorf(stderr, "allowance get: %v", err)
			return 1
		}
		printAllowances(stdout, []client.Allowance{*a})
	case "set":
		a, err := cli.SetAllowance(ctx, name, *fuel, *per)
		if err != nil {
			PrintCLIErrorf(stderr, "allowance set: %v", err)
			return 1
		}
		printAllowances(stdout, []client.Allowance{*a})
	case "delete":
		if err := cli.DeleteAllowance(ctx, name); err != nil {
			PrintCLIErrorf(stderr, "allowance delete: %v", err)
			return 1
		}
		fmt.Fprintf(stdout, "deleted %s (its counters expire with their window)\n", name)
	default:
		var rows []client.Allowance
		cursor := *after
		for {
			page, err := cli.ListAllowances(ctx, cursor, *limit)
			if err != nil {
				PrintCLIErrorf(stderr, "allowance list: %v", err)
				return 1
			}
			rows = append(rows, page.Allowances...)
			cursor = page.Next
			if cursor == "" || !*all {
				break
			}
		}
		if len(rows) == 0 {
			fmt.Fprintf(stderr, "(no allowances in %s)\n", target.Tenant)
			return 0
		}
		printAllowances(stdout, rows)
		if cursor != "" {
			fmt.Fprintf(stderr, "(more — next: --after %q, or --all)\n", cursor)
		}
	}
	return 0
}

func printAllowances(w io.Writer, rows []client.Allowance) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFUEL\tPER\tUSED\tREMAINING\tRESETS")
	for _, a := range rows {
		limit, remaining := "-", "-"
		if a.Defined {
			limit = strconv.FormatInt(a.Fuel, 10)
		}
		if a.Remaining != nil {
			remaining = strconv.FormatInt(*a.Remaining, 10)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", a.Name, limit, a.Per, a.Used, remaining, a.ResetsAt)
	}
	_ = tw.Flush()
}
