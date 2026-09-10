package auth

// Read-only CLI over the remote-source watchers: `txco source status`. Reaches
// the chassis over the signed admin API (GET /v1/tenants/{t}/sources), so it
// works against a remote chassis exactly like `txco auth secrets list`. Sources
// are DECLARED in a stack's OPS/<stack>/SOURCES/ packs, so there is no add/rm
// verb — this only reports cursor, claim, and last-error state. No secret value
// is ever shown (a source config references a secret by name).

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// RunSources dispatches `txco source <sub>` (top-level alias wired in cli.go).
func RunSources(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printSourcesUsage(stdout)
		return 0
	}
	switch args[0] {
	case "status", "list", "ls":
		return runSourcesStatus(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printSourcesUsage(stdout)
		return 0
	default:
		PrintCLIErrorf(stderr, "source: unknown subcommand %q", args[0])
		printSourcesUsage(stderr)
		return 2
	}
}

func printSourcesUsage(w io.Writer) {
	fmt.Fprint(w, `txco source — inspect remote-source watchers

Usage:
  txco source status [flags]   Show each declared source and its poll state

Flags:
  --tenant SLUG    tenant to read (default: the profile's tenant)
  --profile NAME   signing profile
  --target SEL     chassis to act on: a profile name or a raw admin URL
  --url URL        chassis admin endpoint

Sources are declared in a stack's OPS/<stack>/SOURCES/*.jsonl packs and applied
with `+"`txco apply`"+`. This command is read-only: it reports the cursor (how far
each mailbox has been read), which node holds the poll claim, and the last
error, if any. No secret value is ever shown.
`)
}

func runSourcesStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("source status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// A trailing positional selects the target (e.g. `source status cloud`).
	if *targetSel == "" && fs.NArg() > 0 {
		*targetSel = fs.Arg(0)
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "source status: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "source status: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "source status: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	cli := client.New(target)
	rows, err := cli.ListSources(context.Background())
	if err != nil {
		PrintCLIErrorf(stderr, "source status: %v", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stderr, "(no sources declared for %s)\n", target.Tenant)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STACK\tID\tKIND\tSTATE\tCURSOR\tNEXT POLL\tLAST ERROR")
	for _, r := range rows {
		state := r.Status
		switch {
		case r.Retired:
			state = "retired"
		case !r.Enabled:
			state = "disabled"
		}
		cursor := r.Cursor
		if cursor == "" {
			cursor = "-"
		}
		lastErr := r.LastError
		if lastErr == "" {
			lastErr = "-"
		}
		next := r.NextPollAt
		if next == "" {
			next = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Stack, r.DeclaredID, r.Kind, state, cursor, next, lastErr)
	}
	_ = tw.Flush()
	return 0
}
