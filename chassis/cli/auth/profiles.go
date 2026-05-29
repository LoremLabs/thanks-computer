package auth

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// runProfiles renders the list of known profiles, the active one
// first. Mirrors `aws configure list-profiles` in spirit; the
// difference is each row carries a small status snippet so the
// user can tell at a glance which chassis and actor each profile
// is bound to.
func runProfiles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth profiles", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth profiles

List all configured profiles under %[1]s/keys/. The active
profile (selected by %[1]s/active or TXCO_PROFILE env) is
marked with a `+"`*`"+` and sorted first.

Flags:
`, HomePathPretty())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	profs, err := ListProfiles()
	if err != nil {
		fmt.Fprintf(stderr, "auth profiles: %v\n", err)
		return 1
	}
	if len(profs) == 0 {
		fmt.Fprintln(stdout, "(no profiles configured; run `txco auth bootstrap-local` or `txco auth accept`)")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tNAME\tCHASSIS\tACTOR_ID\tKEY_SOURCE")
	for _, p := range profs {
		mark := " "
		if p.Active {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			mark, p.Name, p.Meta.ChassisURL, p.Meta.ActorID, p.Meta.EffectiveKeySource())
	}
	_ = tw.Flush()
	return 0
}
