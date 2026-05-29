package cli

import (
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// runConfig is the `txco config <sub>` alias namespace. It mirrors
// gcloud/kubectl/stripe conventions — the things developers
// instinctively reach for under "config" (active identity, listing
// configured profiles, logging out) live here as aliases to the
// canonical `txco auth ...` subcommands.
//
// Aliasing (rather than re-implementing) means there's one source of
// truth for each verb and back-compat with the auth surface stays
// intact. If we ever migrate non-profile settings into a real config
// file, that work lands here too without re-plumbing dispatch.
func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printConfigUsage(stdout)
		return 0
	}
	switch args[0] {
	case "profile":
		// Forward verbatim — `txco config profile use foo` becomes
		// `txco auth profile use foo`.
		return auth.Dispatch(append([]string{"profile"}, args[1:]...), stdout, stderr)
	case "profiles":
		return auth.Dispatch(append([]string{"profiles"}, args[1:]...), stdout, stderr)
	case "tenants":
		return auth.Dispatch(append([]string{"tenants"}, args[1:]...), stdout, stderr)
	case "tenant":
		return auth.Dispatch(append([]string{"tenant"}, args[1:]...), stdout, stderr)
	case "memberships":
		return auth.Dispatch(append([]string{"memberships"}, args[1:]...), stdout, stderr)
	case "logout":
		return auth.Dispatch(append([]string{"logout"}, args[1:]...), stdout, stderr)
	case "help", "-h", "--help":
		printConfigUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "config: unknown subcommand %q\n\n", args[0])
		printConfigUsage(stderr)
		return 2
	}
}

func printConfigUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco config [flags]
  txco config <command> [flags]

Alias namespace for profile & active identity (gcloud/kubectl/stripe-style).

Examples:
  # See what's configured and what's active
  txco config profiles

  # Switch active identity
  txco config profile use prod

  # Stop signing without removing anything
  txco config logout

Available commands:
  profiles               List configured profiles (alias for `+"`txco auth profiles`"+`)
  profile use <name>     Activate a profile (alias for `+"`txco auth profile use`"+`)
  profile show [<name>]  Print profile details (alias for `+"`txco auth profile show`"+`)
  profile remove <name>  Delete a profile's meta file (alias for `+"`txco auth profile remove`"+`)
  logout                 Stop signing (alias for `+"`txco auth logout`"+`)

These are aliases — anything you can do under `+"`txco config`"+` you can also
do under `+"`txco auth`"+`. Pick whichever namespace fits your muscle memory.

Use `+"`txco config <command> --help`"+` for per-command flags.
`)
}
