package auth

// Read-only CLI over the notebook store: `txco notebook list|read|tail|export`.
// Reaches the chassis over the signed admin API
// (GET /v1/tenants/{t}/notebooks/{ns}[/{name}]), so it works against a
// remote chassis exactly like `txco kv list`. A notebook is addressed by
// its namespace (the app stack by default — `www`, or `pony-<slug>`) and
// its name (`task/42`). Entries print one JSON object per line, oldest
// first; cursors are the opaque strings the chassis returns — pass one back
// with --after, never a sequence number.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// RunNotebook dispatches `txco notebook <sub>` (top-level alias wired in cli.go).
func RunNotebook(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printNotebookUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list", "ls":
		return runNotebookList(args[1:], stdout, stderr)
	case "read":
		return runNotebookRead(args[1:], stdout, stderr, false)
	case "tail":
		return runNotebookRead(args[1:], stdout, stderr, true)
	case "export":
		return runNotebookExport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printNotebookUsage(stdout)
		return 0
	default:
		PrintCLIErrorf(stderr, "notebook: unknown subcommand %q", args[0])
		printNotebookUsage(stderr)
		return 2
	}
}

func printNotebookUsage(w io.Writer) {
	fmt.Fprint(w, `txco notebook — read the append-only notebooks a stack writes (txco://notebook/*)

Usage (flags go before the positionals):
  txco notebook list [flags] <namespace> [prefix]      List notebooks in a namespace
  txco notebook read [flags] <namespace> <name>        Print entries, oldest first (one JSON object per line)
  txco notebook tail [-n N] [flags] <namespace> <name> Print the newest N entries (default 50), oldest first
  txco notebook export [flags] <namespace> <name>      Stream every selected entry as NDJSON to stdout

Flags (all):
  --tenant SLUG    tenant to read (default: the profile's tenant)
  --profile NAME   signing profile
  --target SEL     chassis to act on: a profile name or a raw admin URL
  --url URL        chassis admin endpoint

read / export selection:
  --after CURSOR   resume after this cursor (from a previous page's "next" hint)
  --since RFC3339  entries at or after this time
  --until RFC3339  entries before this time
  --type TYPE      only entries of this type
  --limit N        page size (read) or row cap (export)
  --all            (read) follow pagination and print every entry

The namespace is the app stack by default ("www", or "pony-<slug>" where a
stack chose one); the name is the notebook's own ("task/42"). Entries are
always oldest-to-newest; "seq" is shown but is not a cursor.
`)
}

// notebookTarget resolves the signed target + tenant the way `kv list`
// does, allowing a trailing positional to select the target.
func notebookTarget(cmd string, fs *flag.FlagSet, url, profile, targetSel, tenant *string, positionals int, stderr io.Writer) (*client.Client, string, int) {
	if *targetSel == "" && fs.NArg() > positionals {
		*targetSel = fs.Arg(positionals)
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "notebook %s: %v", cmd, err)
		return nil, "", 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "notebook %s: %v", cmd, err)
		return nil, "", 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIErrorf(stderr, "notebook %s: no signing key configured", cmd)
		return nil, "", 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	return client.New(target), target.Tenant, 0
}

func runNotebookList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("notebook list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	limit := fs.Int("limit", 0, "page size")
	after := fs.String("after", "", "resume after this name")
	all := fs.Bool("all", false, "follow pagination and print every notebook")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "notebook list: NAMESPACE is required")
		printNotebookUsage(stderr)
		return 2
	}
	namespace := fs.Arg(0)
	prefix := ""
	positionals := 1
	if fs.NArg() > 1 {
		prefix = fs.Arg(1)
		positionals = 2
	}
	cli, tenantSlug, code := notebookTarget("list", fs, url, profile, targetSel, tenant, positionals, stderr)
	if code != 0 {
		return code
	}
	ctx := context.Background()
	cursor := *after
	total := 0
	for {
		page, err := cli.ListNotebooks(ctx, namespace, prefix, cursor, *limit)
		if err != nil {
			PrintCLIErrorf(stderr, "notebook list: %v", err)
			return 1
		}
		for _, n := range page.Notebooks {
			fmt.Fprintf(stdout, "%s\t%d\n", n.Name, n.HighSeq)
		}
		total += len(page.Notebooks)
		cursor = page.Next
		if cursor == "" || !*all {
			break
		}
	}
	if total == 0 {
		fmt.Fprintf(stderr, "(no notebooks in %s/%s)\n", tenantSlug, namespace)
		return 0
	}
	if cursor != "" && !*all {
		fmt.Fprintf(stderr, "(more — next: --after %q, or --all)\n", cursor)
	}
	return 0
}

// runNotebookRead prints entries as NDJSON. tail=true is `txco notebook
// tail`: the newest -n entries, still oldest first.
func runNotebookRead(args []string, stdout, stderr io.Writer, tail bool) int {
	cmd := "read"
	if tail {
		cmd = "tail"
	}
	fs := flag.NewFlagSet("notebook "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	opt := client.NotebookReadOptions{}
	fs.StringVar(&opt.After, "after", "", "resume after this cursor")
	fs.StringVar(&opt.Since, "since", "", "entries at or after this RFC 3339 time")
	fs.StringVar(&opt.Until, "until", "", "entries before this RFC 3339 time")
	fs.StringVar(&opt.Type, "type", "", "only entries of this type")
	fs.IntVar(&opt.Limit, "limit", 0, "page size")
	all := fs.Bool("all", false, "follow pagination and print every entry")
	if tail {
		fs.IntVar(&opt.Tail, "n", 50, "number of newest entries")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		PrintCLIErrorf(stderr, "notebook %s: NAMESPACE and NAME are required", cmd)
		printNotebookUsage(stderr)
		return 2
	}
	namespace, name := fs.Arg(0), fs.Arg(1)
	cli, tenantSlug, code := notebookTarget(cmd, fs, url, profile, targetSel, tenant, 2, stderr)
	if code != 0 {
		return code
	}
	ctx := context.Background()
	total := 0
	for {
		page, err := cli.ReadNotebook(ctx, namespace, name, opt)
		if err != nil {
			PrintCLIErrorf(stderr, "notebook %s: %v", cmd, err)
			return 1
		}
		for _, e := range page.Entries {
			fmt.Fprintln(stdout, string(e))
		}
		total += len(page.Entries)
		opt.After = page.Next
		if page.Next == "" || !*all || tail {
			break
		}
	}
	if total == 0 {
		fmt.Fprintf(stderr, "(no entries in %s/%s/%s)\n", tenantSlug, namespace, name)
		return 0
	}
	if opt.After != "" && !*all && !tail {
		fmt.Fprintf(stderr, "(more — next: --after %q, or --all)\n", opt.After)
	}
	return 0
}

func runNotebookExport(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("notebook export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	opt := client.NotebookReadOptions{}
	fs.StringVar(&opt.After, "after", "", "resume after this cursor")
	fs.StringVar(&opt.Since, "since", "", "entries at or after this RFC 3339 time")
	fs.StringVar(&opt.Until, "until", "", "entries before this RFC 3339 time")
	fs.StringVar(&opt.Type, "type", "", "only entries of this type")
	fs.IntVar(&opt.Limit, "limit", 0, "row cap")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		PrintCLIError(stderr, "notebook export: NAMESPACE and NAME are required")
		printNotebookUsage(stderr)
		return 2
	}
	namespace, name := fs.Arg(0), fs.Arg(1)
	cli, _, code := notebookTarget("export", fs, url, profile, targetSel, tenant, 2, stderr)
	if code != 0 {
		return code
	}
	var out io.Writer = stdout
	if stdout == nil {
		out = os.Stdout
	}
	n, err := cli.ExportNotebook(context.Background(), namespace, name, opt, out)
	if err != nil {
		PrintCLIErrorf(stderr, "notebook export: %v", err)
		return 1
	}
	fmt.Fprintf(stderr, "(%d bytes)\n", n)
	return 0
}
