package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runStack: `txco stack <subcommand>` — stack-level settings that live on the
// stack record itself (not on a version). Currently just `set`.
func runStack(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		stackUsage(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "set":
		return runStackSet(rest, stdout, stderr)
	case "-h", "--help", "help":
		stackUsage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "stack: unknown subcommand %q\n", sub)
		stackUsage(stderr)
		return 2
	}
}

func stackUsage(stderr io.Writer) {
	banner.PrintLogo(stderr)
	fmt.Fprint(stderr, `
Usage: txco stack <subcommand> [flags]

Subcommands:
  set    change stack-level settings (e.g. --no-host)

Run `+"`txco stack set --help`"+` for set flags.
`)
}

type stackSetResult struct {
	Stack        string   `json:"stack"`
	MintHostname bool     `json:"mint_hostname"`
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// runStackSet: `txco stack set [--no-host | --host] [--force] <stack>` — flip
// the per-stack auto-URL gate. --no-host makes a later activate mint no public
// routing hostname; --host re-enables it. The stack row is vivified
// server-side, so it can be set BEFORE the stack's first apply (the right time
// — it prevents the URL from ever minting). If the stack already has a live
// URL, --no-host requires --force, which also revokes that URL.
func runStackSet(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("stack set", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	noHost := fs.Bool("no-host", false, "deploy without an auto-minted routing URL (headless)")
	host := fs.Bool("host", false, "re-enable the auto-minted routing URL")
	force := fs.Bool("force", false, "with --no-host, also revoke a URL the stack already has (destructive)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco stack set [--no-host | --host] [--force] <stack>

Flip the per-stack auto-URL gate. By default, activating a web stack mints a
public routing hostname (e.g. <stack>-<rand>.<suffix>). --no-host suppresses
that so the stack deploys with no URL; --host re-enables it.

Set it BEFORE the stack's first apply to prevent the URL from ever minting. If
the stack ALREADY has a live URL, --no-host alone is refused (the URL would keep
serving); add --force to revoke that URL and go headless — this breaks any
existing links to it.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *noHost && *host {
		fmt.Fprintln(stderr, "stack set: pass only one of --no-host / --host")
		return 2
	}
	if !*noHost && !*host {
		fmt.Fprintln(stderr, "stack set: pass --no-host or --host")
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "stack set: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)
	mint := *host // exactly one of the two is set; --host → mint, --no-host → headless

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "stack set: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	res, err := c.SetStackHostMint(context.Background(), stack, mint, *force)
	if err != nil {
		// Tailor the "you must --force" case so the operator sees the live URL
		// and the exact next step rather than a raw 409.
		var he *client.HTTPError
		if errors.As(err, &he) && he.Code == "live_url_exists" {
			fmt.Fprintf(stderr, "stack set: %s already has a live URL%s\n", stack, formatLiveHosts(he.Detail))
			fmt.Fprintln(stderr, "  re-run with --force to revoke it and make the stack headless (breaks existing links).")
			return 1
		}
		fmt.Fprintf(stderr, "stack set: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := writeJSON(stdout, stackSetResult{Stack: stack, MintHostname: res.MintHostname, RevokedHosts: res.RevokedHosts}); err != nil {
			fmt.Fprintf(stderr, "stack set: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if res.MintHostname {
		fmt.Fprintf(stdout, "%s: auto-URL enabled (mints a routing hostname on next activate)\n", stack)
	} else {
		fmt.Fprintf(stdout, "%s: headless (no auto-minted routing URL)\n", stack)
	}
	for _, h := range res.RevokedHosts {
		fmt.Fprintf(stdout, "  revoked %s\n", h)
	}
	return 0
}

// formatLiveHosts renders the hostnames carried in a live_url_exists 409 detail
// as " (host-a, host-b)", or "" if absent/malformed.
func formatLiveHosts(detail map[string]any) string {
	raw, ok := detail["hostnames"].([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			hosts = append(hosts, s)
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	return " (" + strings.Join(hosts, ", ") + ")"
}
