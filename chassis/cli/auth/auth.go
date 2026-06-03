package auth

import (
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// Dispatch routes `txco auth <subcommand> ...`. Status code is what
// the parent CLI returns from os.Exit.
func Dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "bootstrap-local":
		return runBootstrapLocal(args[1:], stdout, stderr)
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "enroll":
		return runEnroll(args[1:], stdout, stderr)
	case "whoami":
		return runWhoami(args[1:], stdout, stderr)
	case "rotate-key":
		return runRotate(args[1:], stdout, stderr)
	case "revoke-key":
		return runRevoke(args[1:], stdout, stderr)
	case "revoke-actor":
		return runRevokeActor(args[1:], stdout, stderr)
	case "invite":
		return runInvite(args[1:], stdout, stderr)
	case "invitations":
		return runInvitations(args[1:], stdout, stderr)
	case "revoke-invitation":
		return runRevokeInvitation(args[1:], stdout, stderr)
	case "accept":
		return runAccept(args[1:], stdout, stderr)
	case "profiles":
		return runProfiles(args[1:], stdout, stderr)
	case "profile":
		return runProfile(args[1:], stdout, stderr)
	case "tenants":
		return runTenants(args[1:], stdout, stderr)
	case "tenant":
		return runTenantCmd(args[1:], stdout, stderr)
	case "memberships":
		return runMemberships(args[1:], stdout, stderr)
	case "logout":
		return runLogout(args[1:], stdout, stderr)
	case "login":
		return runLogin(args[1:], stdout, stderr)
	case "sessions":
		return runSessions(args[1:], stdout, stderr)
	case "secrets":
		return runSecretsCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	banner.PrintLogo(w)
	home := HomePathPretty()
	fmt.Fprintf(w, `
Usage:
  txco auth [flags]
  txco auth <command> [flags]

Manage signing keys for the chassis admin API.

Examples:
  # First-time setup: keygen + enroll + write meta
  txco auth bootstrap-local --secret <s>

  # Invite a teammate (signed); they redeem with `+"`accept`"+`
  txco auth invite --label "alice" --ttl 24h
  txco auth accept --token <TOKEN>

  # Switch active identity / stop signing
  txco auth profile use prod
  txco auth logout

Available commands:
  bootstrap-local           Keygen + enroll + write meta in one shot (recommended for dev)
  init                      Generate a fresh ed25519 keypair under %[1]s/keys/
  enroll                    Enroll an already-generated key against a chassis
  whoami                    Print the chassis's view of the current key's identity
  rotate-key                Generate a new key, enroll it, revoke the old one
  revoke-key                Revoke a specific key (signed)
  revoke-actor              Revoke an actor + cascade to its keys (signed; super_admin)
  invite                    Mint a single-use invitation token for a teammate (signed)
  invitations               List outstanding invitations (signed)
  revoke-invitation         Revoke a pending invitation (signed)
  accept                    Redeem an invitation to mint your own actor + key
  tenants                   List tenants you can see (super_admin sees all)
  tenant create|members     Mint a tenant or list its members
  memberships               List your tenant memberships
  profiles                  List configured profiles (active marked *)
  profile use|show|remove   Activate / inspect / remove a profile
  logout                    Stop signing (sets active profile to "none")
  login                     Mint a browser-login URL (for the admin UI in signed-mode chassis)
  sessions list|revoke      List or revoke browser sessions on the chassis
  secrets init              Mint a host-local master key for the per-tenant secret store

Defaults:
  --url      http://localhost:8081
  --profile  active profile from %[1]s/active, falling back to "local"

Environment:
  TXCO_HOME              base directory (resolved: %[1]s)
  TXCO_PROFILE           override the active profile name (like AWS_PROFILE)
  TXCO_PRIVATE_KEY_PATH  override the private key path for outbound signing
  TXCO_KEY_ID            override the key id used in signatures

Use `+"`txco auth <command> --help`"+` for per-command flags.
`, home)
}
