package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
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
  set    change stack-level settings (e.g. web=false)

Run `+"`txco stack set --help`"+` for set flags.
`)
}

type stackSetResult struct {
	Stack        string   `json:"stack"`
	MintHostname bool     `json:"mint_hostname"`
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// runStackSet: `txco stack set web=true|false [--force] (<stack> | --match
// <substr>)` — turn a stack's own public web URL on or off. The setting is a
// `key=value` positional (config-set style); flags carry only the selector
// (--match) and modifiers (--force/--json). web=false makes a later activate
// mint no public routing hostname; web=true re-enables it. The stack row is
// vivified server-side, so it can be set BEFORE the stack's first apply (the
// right time — it prevents the URL from ever minting). If the stack already has
// a live URL, web=false requires --force, which also revokes that URL.
//
// The surface word is `web` (the outcome — does this stack get its own web
// address); the wire/DB field stays mint_hostname (the mechanism).
func runStackSet(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("stack set", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	force := fs.Bool("force", false, "with web=false, also revoke a URL the stack already has (destructive)")
	match := fs.String("match", "", "apply to every stack whose name CONTAINS this substring (bulk; use instead of a single <stack>), e.g. --match publications")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco stack set web=true|false [--force] (<stack> | --match <substr>)

Turn a stack's own public web URL on or off. web=true mints a routing hostname
(e.g. <stack>-<rand>.<suffix>) on the next activate; web=false suppresses it. A
stack stays reachable via a router either way — this only controls its OWN
auto-minted URL, not all web access.

Set web=false BEFORE a stack's first apply to prevent the URL from ever minting.
If the stack ALREADY has a live URL, web=false alone is refused (the URL would
keep serving); add --force to revoke it and go headless — this breaks any
existing links to it.

--match <substr> applies to EVERY stack whose name contains the substring, in one
server-side transaction — e.g. headless an entire family:
  txco stack set web=false --force --match publications

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Split positionals into the key=value setting(s) and the bare <stack>.
	// Order-independent: `set web=false my-app` ≡ `set my-app web=false`. Only
	// `web` is a setting today; an unknown key or bad bool is a usage error.
	var stackArg string
	var web *bool
	for _, a := range fs.Args() {
		k, v, isKV := strings.Cut(a, "=")
		if !isKV {
			if stackArg != "" {
				fmt.Fprintf(stderr, "stack set: unexpected extra argument %q\n", a)
				return 2
			}
			stackArg = a
			continue
		}
		switch k {
		case "web":
			b, perr := strconv.ParseBool(strings.TrimSpace(v))
			if perr != nil {
				fmt.Fprintf(stderr, "stack set: web= must be true or false, got %q\n", v)
				return 2
			}
			web = &b
		default:
			fmt.Fprintf(stderr, "stack set: unknown setting %q (expected web=true|false)\n", k)
			return 2
		}
	}
	if web == nil {
		fmt.Fprintln(stderr, "stack set: missing setting (expected web=true|false)")
		return 2
	}
	mint := *web // web=true → mint a routing host; web=false → headless

	// Argument-shape errors are usage errors (exit 2) — validate them BEFORE any
	// network setup or the mutation-confirm prompt (which exits 1).
	switch {
	case *match != "" && stackArg != "":
		fmt.Fprintln(stderr, "stack set: pass either a <stack> or --match, not both")
		return 2
	case *match == "" && stackArg == "":
		fmt.Fprintln(stderr, "stack set: missing <stack> argument (or use --match <substr> for bulk)")
		return 2
	}

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "stack set: resolve dir: %v\n", err)
		return 1
	}
	if err := confirmMutationTF(dir, tf, false, stderr); err != nil {
		fmt.Fprintf(stderr, "stack set: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)
	ctx := context.Background()

	// --match: bulk flip across every stack whose name contains the substring,
	// in one server-side tx + reload. (Mutually exclusive with a <stack> — checked above.)
	if *match != "" {
		return runStackSetBatch(ctx, c, *match, mint, *force, *asJSON, stdout, stderr)
	}
	stack := stackArg

	res, err := c.SetStackHostMint(ctx, stack, mint, *force)
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
		fmt.Fprintf(stdout, "%s: web on (mints its own routing URL on next activate)\n", stack)
	} else {
		fmt.Fprintf(stdout, "%s: web off (no own URL; still reachable via a router)\n", stack)
	}
	for _, h := range res.RevokedHosts {
		fmt.Fprintf(stdout, "  revoked %s\n", h)
	}
	return 0
}

type stackSetBatchResult struct {
	Match        string   `json:"match"`
	Matched      int      `json:"matched"`
	MintHostname bool     `json:"mint_hostname"`
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// runStackSetBatch flips the auto-URL gate on every stack whose name contains
// `match`, in one server-side tx. Mirrors the single-stack path's 409 handling
// (here the live-URL conflict names a count + sample of matched stacks).
func runStackSetBatch(ctx context.Context, c *client.Client, match string, mint, force, asJSON bool, stdout, stderr io.Writer) int {
	res, err := c.BatchSetStackHostMint(ctx, match, mint, force)
	if err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.Code == "live_url_exists" {
			fmt.Fprintf(stderr, "stack set: %d matched stack(s) already have a live URL%s\n",
				jsonInt(he.Detail["count"]), formatStringList(he.Detail, "stacks"))
			fmt.Fprintln(stderr, "  re-run with --force to revoke them and make the stacks headless (breaks existing links).")
			return 1
		}
		fmt.Fprintf(stderr, "stack set: %v\n", err)
		return 1
	}
	if asJSON {
		if err := writeJSON(stdout, stackSetBatchResult{
			Match: match, Matched: res.Matched, MintHostname: res.MintHostname, RevokedHosts: res.RevokedHosts,
		}); err != nil {
			fmt.Fprintf(stderr, "stack set: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	state := "web off (no own URL; still reachable via a router)"
	if res.MintHostname {
		state = "web on (mints own URL on next activate)"
	}
	fmt.Fprintf(stdout, "%d stack(s) matching %q → %s\n", res.Matched, match, state)
	if len(res.RevokedHosts) > 0 {
		fmt.Fprintf(stdout, "  revoked %d URL(s)\n", len(res.RevokedHosts))
	}
	return 0
}

// formatLiveHosts renders the hostnames carried in a live_url_exists 409 detail
// as " (host-a, host-b)", or "" if absent/malformed.
func formatLiveHosts(detail map[string]any) string { return formatStringList(detail, "hostnames") }

// formatStringList renders the []string under detail[key] as " (a, b, c)", or ""
// if absent/empty/malformed. Shared by the single-stack and --match 409 paths.
func formatStringList(detail map[string]any, key string) string {
	raw, ok := detail[key].([]any)
	if !ok || len(raw) == 0 {
		return ""
	}
	items := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			items = append(items, s)
		}
	}
	if len(items) == 0 {
		return ""
	}
	return " (" + strings.Join(items, ", ") + ")"
}

// jsonInt coerces a JSON number (decoded as float64) from a 409 detail to int.
func jsonInt(v any) int {
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}
