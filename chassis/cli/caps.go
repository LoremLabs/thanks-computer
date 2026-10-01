package cli

// `txco caps` — the tenant's capability catalogue. A stack declares each
// capability it answers as OPS/<stack>/CAPS/<name>.yaml (chassis/capdecl);
// this lists what the chassis's ACTIVE stacks declare, by name, with the
// stack and scope that answer each: what the capability inlet routes by
// and what txco://caps/list answers to a rule.

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func runCaps(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco caps list [flags]

List the capabilities the tenant's active stacks declare (CAPS/<name>.yaml),
with the stack and scope that answer each.
`)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return runCapsList(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(stderr, "caps: unknown command %q (want: list)\n", args[0])
		return 2
	}
}

func runCapsList(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("caps list", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of the table")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco caps list [flags]

List the capabilities the tenant's active stacks declare (CAPS/<name>.yaml),
with the stack and scope that answer each.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "caps list: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)

	caps, err := c.ListCaps(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "caps list: %v\n", err)
		return 1
	}
	if *asJSON {
		if caps == nil {
			caps = []client.Cap{}
		}
		if err := writeJSON(stdout, caps); err != nil {
			fmt.Fprintf(stderr, "caps list: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(caps) == 0 {
		fmt.Fprintln(stdout, "no capabilities declared")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTACK\tENTRY\tTIMEOUT\tINPUT\tDESCRIPTION")
	for _, cp := range caps {
		if cp.Err != "" {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\tBROKEN: %s\n", cp.Name, cp.Stack, cp.Err)
			continue
		}
		timeout := "-"
		if cp.Timeout > 0 {
			timeout = fmt.Sprintf("%dms", cp.Timeout)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", cp.Name, cp.Stack, cp.Entry, timeout, capInputLabel(cp.Input), capDescLabel(cp.Description))
	}
	_ = tw.Flush()
	return 0
}

// capInputLabel renders a capability's inputs: sorted, a required one
// marked with `*`.
func capInputLabel(input map[string]client.CapInput) string {
	if len(input) == 0 {
		return "-"
	}
	names := make([]string, 0, len(input))
	for n := range input {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		if input[n].Required {
			names[i] = n + "*"
		}
	}
	return strings.Join(names, ",")
}

// capDescLabel is the description's first line, shortened for the table.
func capDescLabel(d string) string {
	d, _, _ = strings.Cut(strings.TrimSpace(d), "\n")
	if r := []rune(d); len(r) > 72 {
		d = string(r[:71]) + "…"
	}
	return d
}
