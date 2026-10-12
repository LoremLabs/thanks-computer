package cli

// `txco calls` — the entries the tenant's stacks declare. A stack declares
// each entry a call may name as OPS/<stack>/CALLS/<name>.yaml
// (chassis/calldecl); this lists what the chassis's ACTIVE stacks declare,
// by stack and name, with where a call enters and what it takes and
// answers: what `WITH entry` resolves through and what txco://calls/list
// answers to a rule.

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func runCalls(args []string, stdout, stderr io.Writer) int {
	usage := func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco calls list [flags]

List the entries the tenant's active stacks declare (CALLS/<name>.yaml),
with where a call enters and what it takes and answers.
`)
	}
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return runCallsList(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		fmt.Fprintf(stderr, "calls: unknown command %q (want: list)\n", args[0])
		return 2
	}
}

func runCallsList(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("calls list", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of the table")
	only := fs.String("stack", "", "list one stack's entries")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco calls list [flags]

List the entries the tenant's active stacks declare (CALLS/<name>.yaml): the
stack, the entry's name, where a call enters, its timeout, and the inputs it
requires and outputs it promises (a required one marked *).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "calls list: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)

	calls, err := c.ListCalls(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "calls list: %v\n", err)
		return 1
	}
	if *only != "" {
		kept := calls[:0]
		for _, cl := range calls {
			if cl.Stack == *only {
				kept = append(kept, cl)
			}
		}
		calls = kept
	}
	if *asJSON {
		if calls == nil {
			calls = []client.Call{}
		}
		if err := writeJSON(stdout, calls); err != nil {
			fmt.Fprintf(stderr, "calls list: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(calls) == 0 {
		fmt.Fprintln(stdout, "no call entries declared")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STACK\tENTRY\tENTERS\tTIMEOUT\tINPUT\tOUTPUT\tDESCRIPTION")
	for _, cl := range calls {
		if cl.Err != "" {
			fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t-\tBROKEN: %s\n", cl.Stack, cl.Name, cl.Err)
			continue
		}
		timeout := "-"
		if cl.Timeout > 0 {
			timeout = fmt.Sprintf("%dms", cl.Timeout)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cl.Stack, cl.Name, cl.Stage, timeout,
			capInputLabel(cl.Input), capInputLabel(cl.Output), capDescLabel(cl.Description))
	}
	_ = tw.Flush()
	return 0
}
