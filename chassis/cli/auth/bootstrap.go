package auth

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// runBootstrapLocal is the friendly one-shot: pick a signing key,
// POST its public half to /auth/dev/enroll, write the returned meta.
//
// Key selection goes through resolveEnrollmentKey (see
// chassis/cli/auth/key_resolve.go) so this command inherits the
// auto-precedence: ssh-agent first, then ~/.ssh/id_ed25519 (with
// TTY confirmation), then a freshly-generated key under $TXCO_HOME.
// Explicit flags (--ssh-agent, --ssh-key, --new-key, --no-ssh-agent)
// short-circuit the auto path.
func runBootstrapLocal(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth bootstrap-local", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultChassisURL, "chassis admin endpoint")
	secret := fs.String("secret", "", "shared dev-enroll secret (prompted on stdin if omitted)")
	homeHint := HomePathPretty()
	profile := fs.String("profile", "", fmt.Sprintf("profile name; meta lands at %s/keys/<profile>.meta.json", homeHint))
	name := fs.String("name", defaultKeyName, "alias for --profile (kept for back-compat)")
	label := fs.String("label", "", "human-readable label stored with the actor")
	kind := fs.String("kind", "human", "actor kind (default human)")
	sshAgent := fs.Bool("ssh-agent", false, "force ssh-agent backend (override auto-detect)")
	noSSHAgent := fs.Bool("no-ssh-agent", false, "skip ssh-agent even when reachable")
	sshKey := fs.String("ssh-key", "", "use an existing on-disk key (e.g. ~/.ssh/id_ed25519)")
	newKey := fs.Bool("new-key", false, fmt.Sprintf("generate a fresh key under %s (skip auto-detect)", homeHint))
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth bootstrap-local --secret <s> [flags]

One-shot: pick a signing key, POST its public half to /auth/dev/enroll,
and write the returned actor_id + key_id to %[1]s/keys/<name>.meta.json.
After this runs, 'txco apply' against the chassis signs requests
automatically.

By default, ssh-agent is preferred when available; otherwise
~/.ssh/id_ed25519 is offered (TTY confirmation); otherwise a fresh
key is generated under %[1]s. --ssh-agent, --ssh-key, --new-key,
--no-ssh-agent steer the selection explicitly.

Flags:
`, homeHint)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --profile is the modern spelling; --name is the back-compat
	// alias. Explicit --profile wins; if neither is set, fall back
	// to the historical default ("local") so existing users don't
	// notice the rename.
	effName := *profile
	if effName == "" {
		effName = *name
	}
	name = &effName

	// Pre-flight: classify the existing on-disk meta (if any) against
	// the target chassis. The four outcomes are:
	//
	//   EnrolmentNone         → first-time setup; fall through.
	//   EnrolmentValid        → the key still works here; exit 0, no
	//                           prompt, no message wall.
	//   EnrolmentRejected     → same chassis, key refused; recovery.
	//   EnrolmentOtherChassis → different chassis URL; recovery.
	//   EnrolmentUnreachable  → couldn't reach the chassis; bail.
	//
	// The "you're already enrolled" case used to dump a wall of
	// invite/rotate/rm hints. With the whoami probe we know whether
	// the user actually needs to do anything — and most of the time
	// they don't.
	recoveredFromCollision := false
	enrolKind, existingMeta, probeErr := classifyExistingEnrolment(*name, *url, probeWhoami)
	switch enrolKind {
	case EnrolmentValid:
		fmt.Fprintf(stdout, "Already enrolled at %s as %s (no action needed).\n",
			*url, existingMeta.ActorID)
		return 0
	case EnrolmentUnreachable:
		PrintCLIErrorf(stderr,
			"auth bootstrap-local: profile %q is bound to %s but the chassis didn't respond: %v",
			*name, *url, probeErr)
		fmt.Fprintln(stderr,
			"  Confirm the chassis is reachable, then rerun. If you want a fresh enrolment regardless, pass --profile=<other>.")
		return 1
	case EnrolmentRejected:
		PrintCLIErrorf(stderr,
			"auth bootstrap-local: profile %q is bound to %s but its key was rejected (the chassis may have been rebuilt, or the key revoked).",
			*name, *url)
		alt, asked := offerAlternateProfile(*name, *url, stderr)
		if !asked {
			fmt.Fprintln(stderr, "  Pass --profile=<other> to enrol against this chassis under a new name.")
			return 1
		}
		if alt == "" {
			return 1
		}
		*name = alt
		recoveredFromCollision = true
	case EnrolmentOtherChassis:
		PrintCLIErrorf(stderr,
			"auth bootstrap-local: profile %q is bound to a different chassis (%s).",
			*name, existingMeta.ChassisURL)
		alt, asked := offerAlternateProfile(*name, *url, stderr)
		if !asked {
			fmt.Fprintln(stderr, "  Pass --profile=<other> to enrol against this chassis without clobbering the existing meta.")
			return 1
		}
		if alt == "" {
			return 1
		}
		*name = alt
		recoveredFromCollision = true
	}

	secretValue, err := resolveSecret(*secret, stderr)
	if err != nil {
		PrintCLIErrorf(stderr, "auth bootstrap-local: %v", err)
		return 2
	}

	ek, err := resolveEnrollmentKey(EnrollmentChoices{
		SSHAgent:   *sshAgent,
		NoSSHAgent: *noSSHAgent,
		SSHKey:     *sshKey,
		NewKey:     *newKey,
		Name:       *name,
	}, os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), stderr)
	if err != nil {
		PrintCLIErrorf(stderr, "auth bootstrap-local: %v", err)
		return 1
	}

	// Default --label from the agent comment when the user didn't
	// supply one. "mattmankins@iMac.lan" beats "" for the actor row
	// the chassis stores; the user can still override explicitly.
	effLabel := *label
	if effLabel == "" {
		effLabel = ek.CommentSuggestion
	}
	resp, err := enroll(*url, secretValue, ek.PublicKey, effLabel, *kind)
	if err != nil {
		// Roll back any freshly-generated artifact so a retry starts clean.
		ek.CleanupOnFailure()
		PrintCLIErrorf(stderr, "auth bootstrap-local: enroll: %v", explainEnrollErr(err))
		return 1
	}

	// Persist the fresh key now that the chassis confirmed enrolment.
	// No-op for ssh-agent / existing-file backends.
	if err := ek.PersistFreshKey(effLabel); err != nil {
		PrintCLIErrorf(stderr, "auth bootstrap-local: persist key: %v", err)
		return 1
	}

	metaPath, err := MetaPath(*name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth bootstrap-local: %v", err)
		return 1
	}
	// DefaultTenant comes from the server response (phase-5 enrolled
	// chassis). On older chassis the field is empty and ResolveTenant
	// will fall through to the literal "default" — same effective
	// behaviour.
	defaultTenant := resp.TenantSlug
	if err := SaveMeta(metaPath, Meta{
		ActorID:       resp.ActorID,
		KeyID:         resp.KeyID,
		ChassisURL:    *url,
		Label:         effLabel,
		EnrolledAt:    time.Now().UTC(),
		KeySource:     ek.KeySource,
		PublicKeyB64:  PublicKeyB64(ek.PublicKey),
		KeyPath:       ek.KeyPath,
		DefaultTenant: defaultTenant,
	}); err != nil {
		PrintCLIErrorf(stderr, "auth bootstrap-local: %v", err)
		return 1
	}

	fmt.Fprintf(stdout, "enrolled %s %s capabilities=%v\n", resp.ActorID, resp.KeyID, resp.Capabilities)
	fmt.Fprintf(stdout, "fingerprint: %s\n", ek.Fingerprint)
	if ek.KeyPath != "" {
		fmt.Fprintf(stdout, "key:  %s\n", ek.KeyPath)
	} else {
		fmt.Fprintf(stdout, "key:  (ssh-agent)\n")
	}
	fmt.Fprintf(stdout, "meta: %s\n", metaPath)
	if defaultTenant != "" {
		fmt.Fprintf(stdout, "tenant: %s", defaultTenant)
		if resp.SuperAdmin {
			fmt.Fprint(stdout, " (super_admin)")
		}
		fmt.Fprintln(stdout)
	}

	// When the user recovered from a collision by picking a new
	// profile name, offer to switch the active profile to it so
	// subsequent commands don't need --profile. We deliberately scope
	// this prompt to the recovery path: a plain `bootstrap-local`
	// run that didn't collide stays silent (no extra prompts to
	// surprise existing users).
	if recoveredFromCollision && term.IsTerminal(int(os.Stdin.Fd())) {
		prompt := fmt.Sprintf("Set profile %q as your default? [Y/n]: ", *name)
		if promptYesNo(os.Stdin, stderr, prompt, true) {
			if err := WriteActiveProfile(*name); err != nil {
				PrintCLIErrorf(stderr, "auth bootstrap-local: set default profile: %v", err)
			} else {
				fmt.Fprintf(stdout, "active profile is now %q\n", *name)
			}
		}
	}

	return 0
}

// offerAlternateProfile asks the user for a different profile name
// when bootstrap-local's preflight detects an existing enrolment.
// Loops until either an enrollable name is provided or the user
// aborts with an empty line.
//
// Returns (chosen, asked). asked=false means stdin isn't a TTY and
// the caller should keep its non-interactive exit-1 behaviour.
// asked=true with chosen=="" means the user explicitly aborted.
func offerAlternateProfile(currentName, urlFlag string, stderr io.Writer) (string, bool) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", false
	}
	suggest := suggestProfileNameFromCwd(currentName)
	for {
		alt, asked, err := promptLine(stderr,
			"Enter an alternate profile name (or press Enter to abort)", suggest)
		if !asked {
			return "", false
		}
		if err != nil {
			PrintCLIErrorf(stderr, "auth bootstrap-local: %v", err)
			return "", true
		}
		if alt == "" {
			return "", true
		}
		if !validKeyName(alt) {
			fmt.Fprintf(stderr,
				"  %q isn't a valid profile name (use letters, digits, '_' or '-'). Try again.\n", alt)
			continue
		}
		// Reject any name that already has a meta on disk — we want a
		// fresh enrolment to land in a clean slot, never to clobber
		// an existing profile (its key might still be valid against
		// some other chassis the user cares about).
		if metaPath, err := MetaPath(alt); err == nil {
			if m, err := LoadMeta(metaPath); err == nil {
				if m.ChassisURL == urlFlag {
					fmt.Fprintf(stderr,
						"  profile %q is already enrolled at %s. Pick another name.\n", alt, urlFlag)
				} else {
					fmt.Fprintf(stderr,
						"  profile %q is already bound to %s. Pick another name.\n", alt, m.ChassisURL)
				}
				// Reset the suggestion so the user doesn't get the same
				// (broken) default prefilled on the next loop.
				suggest = ""
				continue
			}
		}
		return alt, true
	}
}

// suggestProfileNameFromCwd returns the current working directory's
// basename when it'd make a sensible profile name (and isn't already
// the colliding name). Falls through to "" when nothing fits — the
// prompt then renders without a default. The cwd basename is a
// surprisingly good fit for the open-core "I'm experimenting in
// `examples/quickstart`" case, which is the main path this prompt
// helps.
func suggestProfileNameFromCwd(skipName string) string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	base := filepath.Base(wd)
	if !validKeyName(base) {
		return ""
	}
	if base == skipName {
		return ""
	}
	return base
}
