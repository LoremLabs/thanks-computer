package auth

// Read-only CLI over the op-writable KV store: `txco kv list <namespace>`.
// Reaches the chassis over the signed admin API (GET /v1/tenants/{t}/kv/{ns}),
// so it works against a remote chassis exactly like `auth tenant secrets list`.
// The KV namespace is the prefix — e.g. a blog's subscribers live under
// "blog_subscribers"; keys are printed one per line (values are not returned).

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// RunKV dispatches `txco kv <sub>` (top-level alias wired in cli.go).
func RunKV(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printKVUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list", "ls":
		return runKVList(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printKVUsage(stdout)
		return 0
	default:
		PrintCLIErrorf(stderr, "kv: unknown subcommand %q", args[0])
		printKVUsage(stderr)
		return 2
	}
}

func printKVUsage(w io.Writer) {
	fmt.Fprint(w, `txco kv — inspect the op-writable KV store

Usage:
  txco kv list <namespace> [flags]   List the keys stored under a namespace

Flags:
  --tenant SLUG    tenant to read (default: the profile's tenant)
  --profile NAME   signing profile
  --target SEL     chassis to act on: a profile name or a raw admin URL
  --url URL        chassis admin endpoint
  --limit N        page size (max 200; default 200)
  --after CURSOR   resume after this key (from a previous page's "next" hint)
  --all            follow pagination and print every key

The KV namespace is the prefix — e.g. a blog's subscribers live under
"blog_subscribers". Keys print one per line; values are not returned (fetch a
value with its key via the app).
`)
}

func runKVList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kv list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	limit := fs.Int("limit", 0, "page size (max 200)")
	after := fs.String("after", "", "resume cursor")
	all := fs.Bool("all", false, "follow pagination and print every key")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "kv list: NAMESPACE is required")
		printKVUsage(stderr)
		return 2
	}
	namespace := fs.Arg(0)
	// A trailing positional after the namespace selects the target (e.g.
	// `kv list blog_subscribers cloud`), mirroring the secrets commands.
	if *targetSel == "" && fs.NArg() > 1 {
		*targetSel = fs.Arg(1)
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "kv list: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "kv list: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "kv list: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	cli := client.New(target)
	ctx := context.Background()
	cursor := *after
	total := 0
	for {
		page, err := cli.ListKV(ctx, namespace, cursor, *limit)
		if err != nil {
			PrintCLIErrorf(stderr, "kv list: %v", err)
			return 1
		}
		for _, k := range page.Keys {
			fmt.Fprintln(stdout, k)
		}
		total += len(page.Keys)
		cursor = page.Next
		if cursor == "" || !*all {
			break
		}
	}
	if total == 0 {
		fmt.Fprintf(stderr, "(no keys in %s/%s)\n", target.Tenant, namespace)
		return 0
	}
	if cursor != "" && !*all {
		// Windowed page: show how to continue without dumping everything.
		fmt.Fprintf(stderr, "(more — next: --after %q, or --all)\n", cursor)
	}
	return 0
}
