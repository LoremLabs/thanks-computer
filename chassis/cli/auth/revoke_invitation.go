package auth

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runRevokeInvitation marks a pending invitation as revoked. After
// this, any caller trying to consume that token gets 404. Signed.
func runRevokeInvitation(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth revoke-invitation", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	name := fs.String("name", defaultKeyName, fmt.Sprintf("signing key name under %s/keys/", HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug the invitation belongs to (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth revoke-invitation <invitation_id> [flags]

Revoke a pending invitation by id. The id is printed when the
invitation is created; 'txco auth invitations' lists current ones.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth revoke-invitation: invitation id is required")
		fs.Usage()
		return 2
	}
	invID := fs.Arg(0)

	target, err := buildSignedTarget(*name, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth revoke-invitation: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth revoke-invitation: no signing key configured; revoke requires authentication")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, *name)

	if err := ConfirmTargetStd(*name, target.Addr, *yes, false, stderr); err != nil {
		fmt.Fprintf(stderr, "auth revoke-invitation: %v\n", err)
		return 1
	}

	resp, err := client.New(target).RevokeInvitation(context.Background(), invID)
	if err != nil {
		fmt.Fprintf(stderr, "auth revoke-invitation: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", resp.InvitationID)
	return 0
}
