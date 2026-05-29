package auth

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// runProfile dispatches `txco auth profile <sub>` into use / show /
// remove handlers. Kept as a sub-dispatcher (rather than three
// flat verbs at the top level) so the `profile` namespace stays
// tidy and the help text groups related verbs together.
func runProfile(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printProfileUsage(stdout)
		return 0
	}
	switch args[0] {
	case "use":
		return runProfileUse(args[1:], stdout, stderr)
	case "show":
		return runProfileShow(args[1:], stdout, stderr)
	case "remove", "rm":
		return runProfileRemove(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printProfileUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth profile: unknown subcommand %q\n\n", args[0])
		printProfileUsage(stderr)
		return 2
	}
}

func printProfileUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprintf(w, `
Usage:
  txco auth profile [flags]
  txco auth profile <command> [flags]

Manage named identities (AWS-style profiles).

Examples:
  # Switch active identity
  txco auth profile use prod

  # See what the active profile points at
  txco auth profile show

  # Forget a profile (key file is left alone)
  txco auth profile remove old-laptop

Available commands:
  use <name>      Set <name> as the active profile (persisted in %[1]s/active)
  show [<name>]   Print details for one profile (defaults to the active one)
  remove <name>   Remove a profile's meta file — key files are NEVER touched

Related:
  txco auth profiles    List all configured profiles
  txco auth logout      Stop signing without removing anything

Selection precedence at command time (highest first):
  --profile flag → TXCO_PROFILE env → %[1]s/active → "local"
`, HomePathPretty())
}

// --- profile use ----------------------------------------------------------

// runProfileUse writes the active-profile pointer. Verifies the
// target profile actually exists (a meta file) before persisting —
// no point activating a name that won't resolve at sign time.
func runProfileUse(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth profile use", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth profile use <name>

Set <name> as the active profile. Future signed commands without
--profile or TXCO_PROFILE will use this identity.

The name must refer to an existing profile (a %[1]s/keys/<name>.meta.json).
Run `+"`txco auth profiles`"+` to see what's available.

Flags:
`, HomePathPretty())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth profile use: profile name is required")
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)

	// Verify the profile exists before activating — otherwise the
	// next command silently fails with "no signing key" and the
	// user has to guess why.
	mp, err := MetaPath(name)
	if err != nil {
		fmt.Fprintf(stderr, "auth profile use: %v\n", err)
		return 1
	}
	if _, err := LoadMeta(mp); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "auth profile use: profile %q not found (no %s).\n", name, mp)
			fmt.Fprintln(stderr, "  Run `txco auth profiles` to list, or `txco auth bootstrap-local --profile "+name+"` to create.")
			return 1
		}
		fmt.Fprintf(stderr, "auth profile use: load meta: %v\n", err)
		return 1
	}
	if err := WriteActiveProfile(name); err != nil {
		fmt.Fprintf(stderr, "auth profile use: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "active profile: %s\n", name)
	return 0
}

// --- profile show ---------------------------------------------------------

// runProfileShow prints the meta details for a named profile. With
// no argument, shows whichever profile is currently active —
// helpful for "what am I signing as right now?" answers without
// pulling out `cat` and a JSON pretty-printer.
func runProfileShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth profile show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth profile show [<name>]

Print profile details. With no argument, shows the active profile.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	name := ""
	if fs.NArg() >= 1 {
		name = fs.Arg(0)
	}
	if name == "" {
		active, err := ReadActiveProfile()
		if err != nil {
			fmt.Fprintf(stderr, "auth profile show: %v\n", err)
			return 1
		}
		if active == ActiveNone {
			fmt.Fprintln(stdout, "no active profile (logged out)")
			fmt.Fprintln(stdout, "run `txco auth profile use <name>` or `txco auth bootstrap-local` to set one")
			return 0
		}
		name = active
	}

	mp, err := MetaPath(name)
	if err != nil {
		fmt.Fprintf(stderr, "auth profile show: %v\n", err)
		return 1
	}
	m, err := LoadMeta(mp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "auth profile show: profile %q not found (no %s)\n", name, mp)
			return 1
		}
		fmt.Fprintf(stderr, "auth profile show: %v\n", err)
		return 1
	}

	active, _ := ReadActiveProfile()
	fmt.Fprintf(stdout, "profile:      %s\n", name)
	if active == name {
		fmt.Fprintln(stdout, "status:       active")
	} else {
		fmt.Fprintln(stdout, "status:       inactive")
	}
	fmt.Fprintf(stdout, "chassis_url:  %s\n", m.ChassisURL)
	fmt.Fprintf(stdout, "actor_id:     %s\n", m.ActorID)
	fmt.Fprintf(stdout, "key_id:       %s\n", m.KeyID)
	if m.Label != "" {
		fmt.Fprintf(stdout, "label:        %s\n", m.Label)
	}
	fmt.Fprintf(stdout, "key_source:   %s\n", m.EffectiveKeySource())
	if m.KeyPath != "" {
		fmt.Fprintf(stdout, "key_path:     %s\n", m.KeyPath)
	}
	if !m.EnrolledAt.IsZero() {
		fmt.Fprintf(stdout, "enrolled_at:  %s\n", m.EnrolledAt.Format("2006-01-02T15:04:05Z"))
	}
	fmt.Fprintf(stdout, "meta:         %s\n", mp)
	return 0
}

// --- profile remove -------------------------------------------------------

// runProfileRemove deletes a profile's meta file ONLY. The key
// material is never touched — it might be shared with ssh-agent,
// `~/.ssh/id_ed25519`, or other tooling. If the user wants the
// key gone too, they `rm` it explicitly. We print the path of the
// (untouched) key file so they know what we left behind.
func runProfileRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth profile remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("y", false, "skip the confirmation prompt")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth profile remove <name>

Remove the named profile's meta file from %[1]s/keys/. The
key material it points at is NEVER touched — that key may be in use
by ssh-agent, ssh, or other tooling. The CLI prints the key path
so you can `+"`rm`"+` it yourself if you also want it gone.

If <name> is the active profile, the active pointer is reset to
"none" (effectively a logout).

Flags:
`, HomePathPretty())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth profile remove: profile name is required")
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)

	mp, err := MetaPath(name)
	if err != nil {
		fmt.Fprintf(stderr, "auth profile remove: %v\n", err)
		return 1
	}
	m, err := LoadMeta(mp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "auth profile remove: profile %q not found (no %s)\n", name, mp)
			return 1
		}
		fmt.Fprintf(stderr, "auth profile remove: %v\n", err)
		return 1
	}

	// Surface what we'll touch and what we won't BEFORE asking
	// for confirmation. Users sometimes forget which path is the
	// key vs the meta; spell it out.
	keyPath := m.KeyPath
	if keyPath == "" && m.EffectiveKeySource() == SourceFile {
		// Legacy meta (no KeyPath field): the key lives at the
		// canonical path next to the meta.
		keyPath = strings.TrimSuffix(mp, ".meta.json")
	}
	fmt.Fprintf(stderr, "will remove: %s\n", mp)
	if keyPath != "" {
		fmt.Fprintf(stderr, "will NOT touch key file: %s\n", keyPath)
		fmt.Fprintln(stderr, "  (rm it yourself if you also want the key gone)")
	}

	if !*force {
		if !promptYesNo(os.Stdin, stderr, "proceed? [y/N]: ", false) {
			fmt.Fprintln(stdout, "aborted")
			return 0
		}
	}

	if err := os.Remove(mp); err != nil {
		fmt.Fprintf(stderr, "auth profile remove: %v\n", err)
		return 1
	}

	// If we just removed the ACTIVE profile, drop the active
	// pointer to ActiveNone — otherwise next command would point
	// at a non-existent meta and confuse the loader.
	active, _ := ReadActiveProfile()
	if active == name {
		if err := WriteActiveProfile(ActiveNone); err != nil {
			fmt.Fprintf(stderr, "  (warning: couldn't clear active pointer: %v)\n", err)
		} else {
			fmt.Fprintln(stdout, "(was active — switched to no-profile / unsigned)")
		}
	}

	fmt.Fprintf(stdout, "removed profile %s\n", name)
	return 0
}

// --- logout ---------------------------------------------------------------

// runLogout is the friendly alias for `txco auth profile use none`.
// Same effect: active pointer is set to ActiveNone, so subsequent
// commands send unsigned (chassis in auth-mode=both or open will
// accept; signed-only chassis will refuse, which is the expected
// "you're logged out" signal).
//
// Key files and meta files are ALL untouched — this is a soft
// logout, not a destroy operation.
func runLogout(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth logout

Stop signing requests. Sets the active profile to "none" so
subsequent commands send unsigned (which open-mode and auth-mode=both
chassis accept; signed-only chassis return 401). NO files are
removed — log back in any time with `+"`txco auth profile use <name>`"+`.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	prev, _ := ReadActiveProfile()
	if err := WriteActiveProfile(ActiveNone); err != nil {
		fmt.Fprintf(stderr, "auth logout: %v\n", err)
		return 1
	}
	if prev != ActiveNone && prev != "" {
		fmt.Fprintf(stdout, "logged out (was %q; meta files untouched)\n", prev)
	} else {
		fmt.Fprintln(stdout, "logged out (already had no active profile)")
	}
	return 0
}
