package cli

// `txco runs` — the tenant's runs in flight on the chassis — and `txco
// abort` — end one of them, or every run of a stack, without ending the
// chassis. An abort is a cancel of the run's context: ops in flight stop,
// nothing later in the run executes, and the run's trace says `aborted`
// and by whom (chassis/processor/liveruns.go, admin/runs.go).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func runRuns(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("runs", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	stack := fs.String("stack", "", "only runs that entered at, or are now in, this stack")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of the table")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco runs [flags]

List the tenant's runs in flight on the chassis: the request id, the inlet it
came in by, the stack it was routed into, the scope it is in now, and how long
it has been running. End one with: txco abort <rid>

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "runs: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)

	runs, err := c.ListRuns(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "runs: %v\n", err)
		return 1
	}
	if *stack != "" {
		kept := runs[:0]
		for _, r := range runs {
			if r.Entry == *stack || r.Stack == *stack {
				kept = append(kept, r)
			}
		}
		runs = kept
	}
	if *asJSON {
		if err := writeJSON(stdout, runs); err != nil {
			fmt.Fprintf(stderr, "runs: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "no runs in flight")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RID\tSRC\tENTRY\tSTAGE\tAGE\tSTATUS")
	for _, r := range runs {
		status := "running"
		if r.AbortedBy != "" {
			status = "aborting (" + r.AbortedBy + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.RID, dashIfEmpty(r.Src), dashIfEmpty(r.Entry), dashIfEmpty(r.Stage), ageLabel(r.AgeMs), status)
	}
	_ = tw.Flush()
	return 0
}

func runAbort(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("abort", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	stack := fs.String("stack", "", "end every run in flight that entered at, or is now in, this stack")
	reason := fs.String("reason", "", "why, for the run's trace and the chassis log")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco abort <rid> [flags]
       txco abort --stack <name> [flags]

End a run in flight on the chassis (see: txco runs), or every run of a stack,
without ending the chassis. A hard stop: the ops in flight are cancelled, nothing
later in the run executes, and its trace says "aborted" and by whom. A stack
that must tidy up does so before it asks (EXEC "txco://run/abort"). To keep a
stack from starting new runs as well, deactivate it first.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	switch {
	case *stack == "" && len(rest) != 1:
		fs.Usage()
		return 2
	case *stack != "" && len(rest) != 0:
		fmt.Fprintln(stderr, "abort: give a run id or --stack, not both")
		return 2
	}

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "abort: resolve dir: %v\n", err)
		return 1
	}
	if err := confirmMutationTF(dir, tf, *asJSON, stderr); err != nil {
		fmt.Fprintf(stderr, "abort: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)

	var res client.AbortResult
	if *stack != "" {
		res, err = c.AbortStack(context.Background(), *stack, *reason)
	} else {
		res, err = c.AbortRun(context.Background(), rest[0], *reason)
	}
	if err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == http.StatusNotFound && he.Code == "run_not_live" {
			fmt.Fprintf(stderr, "abort: no run %s is in flight on this chassis (it may have finished, or be on another node)\n", rest[0])
			return 1
		}
		fmt.Fprintf(stderr, "abort: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := writeJSON(stdout, res); err != nil {
			fmt.Fprintf(stderr, "abort: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	// On a fleet the admin plane aborts what it holds and sends the rest
	// to every node as a control event; the count is this process's.
	fleet := ""
	if res.Published {
		fleet = " here; sent to every node of the fleet"
	}
	switch {
	case *stack != "" && res.Aborted == 0 && res.Published:
		fmt.Fprintf(stdout, "no runs of %s in flight here; sent to every node of the fleet\n", *stack)
	case *stack != "" && res.Aborted == 0:
		fmt.Fprintf(stdout, "no runs of %s in flight\n", *stack)
	case *stack != "":
		fmt.Fprintf(stdout, "aborted %d run(s) of %s%s\n", res.Aborted, *stack, fleet)
	case res.Aborted == 0 && res.Published:
		fmt.Fprintf(stdout, "%s is not in flight here; sent to every node of the fleet (the one that holds it ends it)\n", rest[0])
	default:
		fmt.Fprintf(stdout, "aborted %s%s\n", rest[0], fleet)
	}
	return 0
}

// ageLabel renders a run's age the way a person reads it: 850ms, 12.3s, 4m05s.
func ageLabel(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}
