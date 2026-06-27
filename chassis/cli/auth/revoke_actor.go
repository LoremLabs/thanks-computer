package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runRevokeActor revokes an actor by id. The server's RevokeActor
// stamps actors.revoked_at and cascades to actor_keys; subsequent
// signed calls from any of the actor's keys land on the middleware's
// actor_revoked response. Capability gate: actor:*:revoke (super_admin),
// enforced server-side.
//
// Self-revoke is refused both client-side (whoami compare) and server-
// side (409). The client check is ergonomics — it costs one round-trip
// and turns a destructive accident into a typo. The server check is the
// load-bearing guard.
func runRevokeActor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth revoke-actor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	name := fs.String("name", defaultKeyName, fmt.Sprintf("signing key name under %s/keys/", HomePathPretty()))
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug the actor belongs to (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	actorIDFlag := fs.String("actor-id", "", "actor id to revoke (required; or pass as positional arg)")
	iAmSure := fs.Bool("i-am-sure", false, "confirm self-revoke (otherwise refused to prevent bricking the active key)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth revoke-actor <actor-id> [flags]

Revoke an actor by id. Stamps actors.revoked_at and cascades to all
the actor's keys; subsequent signed calls fail with actor_revoked.
Soft-revoke only — the row stays for forensics.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	targetID := *actorIDFlag
	if targetID == "" && fs.NArg() >= 1 {
		targetID = fs.Arg(0)
	}
	if targetID == "" {
		fmt.Fprintln(stderr, "auth revoke-actor: actor id is required (pass --actor-id or as positional arg)")
		fs.Usage()
		return 2
	}

	applyTargetSelectorName(*targetSel, url, name)
	target, err := buildSignedTarget(*name, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth revoke-actor: %v\n", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		fmt.Fprintln(stderr, "auth revoke-actor: no signing key configured; revoke requires authentication")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, *name)
	if err := ConfirmTargetStd(*name, target.Addr, *yes, false, stderr); err != nil {
		fmt.Fprintf(stderr, "auth revoke-actor: %v\n", err)
		return 1
	}

	ctx := context.Background()
	c := client.New(target)

	// Self-revoke guard. Whoami uses the same signed target we're about
	// to revoke with — if the server agrees that targetID is us and
	// --i-am-sure isn't set, refuse before posting the destructive call.
	if !*iAmSure {
		me, err := c.Whoami(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "auth revoke-actor: whoami failed (cannot perform self-revoke check): %v\n", err)
			return 1
		}
		if me.ActorID == targetID {
			fmt.Fprintf(stderr,
				"auth revoke-actor: %q is your own actor; revoking yourself will cascade to all your keys.\n"+
					"  Pass --i-am-sure to confirm (the server will still refuse; have a peer super_admin revoke you instead).\n",
				targetID)
			return 2
		}
	}

	resp, err := c.RevokeActor(ctx, targetID)
	if err != nil {
		// Surface the server's hint on the 409 self-revoke path so the
		// user sees actionable guidance even if they bypassed the
		// client-side guard (--i-am-sure, or stale whoami).
		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == 409 {
			fmt.Fprintf(stderr, "auth revoke-actor: %s", he.Code)
			if hint, ok := he.Detail["hint"].(string); ok && hint != "" {
				fmt.Fprintf(stderr, " — %s", hint)
			}
			fmt.Fprintln(stderr)
			return 1
		}
		fmt.Fprintf(stderr, "auth revoke-actor: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", resp.ActorID)
	return 0
}
