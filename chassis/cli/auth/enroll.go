package auth

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/term"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

const defaultChassisURL = "http://localhost:8081"

// runEnroll enrolls a public key with the chassis. Differs from
// `bootstrap-local` and `accept` in that the default behaviour is
// "use the existing key at $TXCO_HOME/keys/<name>.ed25519" rather
// than auto-detecting ssh-agent. This preserves the v1 enroll
// semantics ("I already have a key, enroll it") while allowing
// users to opt into the other backends explicitly.
//
//	no flag        → file at $TXCO_HOME/keys/<name>.ed25519 (current)
//	--ssh-agent    → enroll an ssh-agent key
//	--ssh-key PATH → enroll an existing key at PATH
//
// If you want full auto-detect (ssh-agent first, etc.), use
// `bootstrap-local` instead.
func runEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultChassisURL, "chassis admin endpoint")
	secret := fs.String("secret", "", "shared dev-enroll secret (prompted on stdin if omitted)")
	homeHint := HomePathPretty()
	profile := fs.String("profile", "", fmt.Sprintf("profile name; meta lands at %s/keys/<profile>.meta.json", homeHint))
	name := fs.String("name", defaultKeyName, "alias for --profile (kept for back-compat)")
	label := fs.String("label", "", "human-readable label stored with the actor")
	kind := fs.String("kind", "human", "actor kind (default human)")
	sshAgent := fs.Bool("ssh-agent", false, "enroll an ssh-agent key instead of the on-disk one")
	sshKey := fs.String("ssh-key", "", "enroll an existing on-disk key at this path")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth enroll --secret <s> [flags]

POST a public key's bytes to /auth/dev/enroll and write the
returned actor_id + key_id to <name>.meta.json. By default uses
the on-disk key at %[1]s/keys/<name>.ed25519; pass --ssh-agent
or --ssh-key to enroll a different key without generating one.

Flags:
`, homeHint)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --profile wins over --name (back-compat alias).
	if *profile != "" {
		*name = *profile
	}

	secretValue, err := resolveSecret(*secret, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "auth enroll: %v\n", err)
		return 2
	}

	// Default to the on-disk key under $TXCO_HOME unless the user
	// asked for a different backend. Note: NewKey is false here —
	// enroll never generates a key (that's bootstrap-local's job).
	choices := EnrollmentChoices{
		SSHAgent: *sshAgent,
		SSHKey:   *sshKey,
		Name:     *name,
		Label:    *label,
	}
	if !*sshAgent && *sshKey == "" {
		kp, err := KeyPath(*name)
		if err != nil {
			fmt.Fprintf(stderr, "auth enroll: %v\n", err)
			return 1
		}
		if _, statErr := os.Stat(kp); errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintf(stderr, "auth enroll: key %q not found; run `txco auth init --name %s` or `txco auth bootstrap-local` first\n", kp, *name)
			return 1
		}
		choices.SSHKey = kp
	}

	ek, err := resolveEnrollmentKey(choices,
		os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "auth enroll: %v\n", err)
		return 1
	}

	effLabel := *label
	if effLabel == "" {
		effLabel = ek.CommentSuggestion
	}
	resp, err := enroll(*url, secretValue, ek.PublicKey, effLabel, *kind)
	if err != nil {
		ek.CleanupOnFailure()
		fmt.Fprintf(stderr, "auth enroll: %v\n", explainEnrollErr(err))
		return 1
	}
	if err := ek.PersistFreshKey(effLabel); err != nil {
		fmt.Fprintf(stderr, "auth enroll: persist key: %v\n", err)
		return 1
	}

	metaPath, err := MetaPath(*name)
	if err != nil {
		fmt.Fprintf(stderr, "auth enroll: %v\n", err)
		return 1
	}
	if err := SaveMeta(metaPath, Meta{
		ActorID:      resp.ActorID,
		KeyID:        resp.KeyID,
		ChassisURL:   *url,
		Label:        effLabel,
		EnrolledAt:   time.Now().UTC(),
		KeySource:    ek.KeySource,
		PublicKeyB64: PublicKeyB64(ek.PublicKey),
		KeyPath:      ek.KeyPath,
	}); err != nil {
		fmt.Fprintf(stderr, "auth enroll: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "enrolled %s %s capabilities=%v\n", resp.ActorID, resp.KeyID, resp.Capabilities)
	fmt.Fprintf(stdout, "fingerprint: %s\n", ek.Fingerprint)
	fmt.Fprintf(stdout, "meta: %s\n", metaPath)
	return 0
}

// enroll is a small helper used by `enroll`, `bootstrap-local`, and
// `rotate-key`. Kept package-local because nothing outside this
// package wants the raw transport.
func enroll(url, secret string, pub ed25519.PublicKey, label, kind string) (*client.DevEnrollResponse, error) {
	c := client.New(client.Target{Addr: url})
	return c.DevEnroll(context.Background(), secret, client.DevEnrollRequest{
		PublicKeyB64: PublicKeyB64(pub),
		Algorithm:    "ed25519",
		Label:        label,
		Kind:         kind,
	})
}
