package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runDNS routes `txco dns <subcommand> ...`.
func runDNS(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printDNSUsage(stdout)
		return 0
	}
	switch args[0] {
	case "render":
		return runDNSRender(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printDNSUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "dns: unknown subcommand %q\n\n", args[0])
		printDNSUsage(stderr)
		return 2
	}
}

func printDNSUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco dns <subcommand> [flags]

Subcommands:
  render    Print the authoritative zone(s) the chassis would serve for
            your tenant, in zone-file form.

Run 'txco dns render --help' for flags.
`)
}

// runDNSRender prints the rendered zone-file(s) for the tenant — the
// same ZoneSnapshot the dns head answers from, fetched from the running
// chassis. Useful to preview a zone before delegating and to debug the
// records that would be served.
func runDNSRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml")
	addr := fs.String("addr", "", "chassis admin endpoint")
	user := fs.String("user", "", "basic auth user")
	pass := fs.String("pass", "", "basic auth password")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug")
	zone := fs.String("zone", "", "render only this origin (default: all the tenant's zones)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco dns render [flags] [<zone>]

Print the authoritative DNS zone(s) the chassis would serve for your
tenant, in standard zone-file form — the same records the dns head
answers from. Preview a zone before delegating, or debug synthesized
records. A positional <zone> (or --zone) limits output to one origin.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Accept the origin as a positional too: `txco dns render ops.example.com`.
	z := *zone
	if z == "" {
		z = fs.Arg(0)
	}

	clientTarget := resolveTarget(".", *target, *addr, *user, *pass, *profile)
	clientTarget.Tenant = resolveTenant(*tenant, *profile)
	c := client.New(clientTarget)

	out, err := c.GetDNSRender(context.Background(), z)
	if err != nil {
		fmt.Fprintf(stderr, "dns render: %v\n", err)
		return 1
	}
	if strings.TrimSpace(out) == "" {
		fmt.Fprintln(stderr, "dns render: no zones served for this tenant")
		return 0
	}
	fmt.Fprint(stdout, out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(stdout)
	}
	return 0
}
