package auth

import (
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runInvitations lists all invitations the chassis knows about, with
// derived status (active / consumed / revoked / expired). Signed.
func runInvitations(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth invitations", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	name := fs.String("name", defaultKeyName, fmt.Sprintf("signing key name under %s/keys/", HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug to list invitations for (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth invitations [flags]

List invitations on the chassis with their current status.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target, err := buildSignedTarget(*name, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth invitations: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth invitations: no signing key configured; listing requires authentication")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, *name)

	invs, err := client.New(target).ListInvitations(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "auth invitations: %v\n", err)
		return 1
	}

	if len(invs) == 0 {
		fmt.Fprintln(stdout, "(no invitations)")
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tLABEL\tCREATED_BY\tEXPIRES_AT\tCONSUMED_BY")
	for _, i := range invs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			i.InvitationID, i.Status, dashIfEmpty(i.Label), i.CreatedBy, i.ExpiresAt, dashIfEmpty(i.ConsumedBy))
	}
	_ = tw.Flush()
	return 0
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
