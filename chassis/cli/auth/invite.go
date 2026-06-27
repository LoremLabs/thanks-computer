package auth

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runInvite mints a new single-use invitation token. Signed call —
// the chassis enforces actor:invite capability (satisfied by
// admin:all today). Prints the raw token exactly once.
func runInvite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth invite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	name := fs.String("name", defaultKeyName, fmt.Sprintf("signing key name under %s/keys/", HomePathPretty()))
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug to invite into (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	label := fs.String("label", "", "label stored with the invitation (visible to the inviter and on the new actor)")
	kind := fs.String("kind", "human", "actor kind written to the new actor row")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the token is valid; server caps at 7d")
	caps := fs.String("caps", "", "comma-separated capabilities the invitee receives (e.g. \"opstack:*:read,actor:*:read\"); empty defaults to admin:all")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth invite [flags]

Mint a single-use invitation token that a teammate can redeem with
'txco auth accept'. The token is printed once; share it via Slack or
email. Past the TTL or once consumed, the invitation can no longer
be redeemed.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	applyTargetSelectorName(*targetSel, url, name)
	target, err := buildSignedTarget(*name, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth invite: %v\n", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		fmt.Fprintln(stderr, "auth invite: no signing key configured; inviting requires authentication")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, *name)

	// Parse + validate --caps client-side so a typo errors out before
	// we burn an http round-trip. Empty input → nil slice; server
	// defaults to admin:all in that case.
	parsedCaps, err := policy.ParseCapabilities(*caps)
	if err != nil {
		fmt.Fprintf(stderr, "auth invite: %v\n", err)
		return 2
	}

	if err := ConfirmTargetStd(*name, target.Addr, *yes, false, stderr); err != nil {
		fmt.Fprintf(stderr, "auth invite: %v\n", err)
		return 1
	}

	resp, err := client.New(target).CreateInvitation(context.Background(), client.CreateInvitationRequest{
		Label:        *label,
		Kind:         *kind,
		TTLSeconds:   int(ttl.Seconds()),
		Capabilities: parsedCaps,
	})
	if err != nil {
		fmt.Fprintf(stderr, "auth invite: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "invitation_id: %s\n", resp.InvitationID)
	fmt.Fprintf(stdout, "token:         %s\n", resp.Token)
	fmt.Fprintf(stdout, "expires_at:    %s\n", resp.ExpiresAt)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "share with the invitee — they run: txco auth accept")
	return 0
}
