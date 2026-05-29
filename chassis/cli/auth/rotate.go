package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// runRotate generates a new keypair, enrolls it (yielding a new
// key_id), then revokes the old key using the OLD key's signature.
// Order matters: revoke last, so the old key remains valid until the
// new one is provably accepted. If enrollment fails we don't touch
// the old key.
//
// Note: this currently creates a *new actor* (dev-enroll always does).
// True same-actor rotation needs a registered "rotate" endpoint that
// the v1 server doesn't expose; deferred to a follow-up.
func runRotate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth rotate-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	secret := fs.String("secret", "", "shared dev-enroll secret (prompted on stdin if omitted)")
	name := fs.String("name", defaultKeyName, fmt.Sprintf("key name under %s/keys/", HomePathPretty()))
	label := fs.String("label", "", "human-readable label stored with the new actor")
	kind := fs.String("kind", "human", "actor kind (default human)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth rotate-key --secret <s> [flags]

Generate a new keypair, enroll it, then revoke the old key. The old
private key file is moved aside to <name>.ed25519.rotated-<unix> so
nothing is lost if the new key isn't usable yet.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	secretValue, err := resolveSecret(*secret, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: %v\n", err)
		return 2
	}

	keyPath, err := KeyPath(*name)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: %v\n", err)
		return 1
	}
	metaPath, err := MetaPath(*name)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: %v\n", err)
		return 1
	}
	oldMeta, err := LoadMeta(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "auth rotate-key: no enrolled meta at %q; nothing to rotate. Run `txco auth bootstrap-local` first.\n", metaPath)
			return 1
		}
		fmt.Fprintf(stderr, "auth rotate-key: %v\n", err)
		return 1
	}
	oldPriv, err := LoadPrivateKey(keyPath)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: %v\n", err)
		return 1
	}
	chassisURL := *url
	if chassisURL == "" {
		chassisURL = oldMeta.ChassisURL
	}
	if chassisURL == "" {
		chassisURL = defaultChassisURL
	}

	// Generate + enroll the new keypair OUT-OF-BAND first.
	newPub, newPriv, err := GenerateKey()
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: generate: %v\n", err)
		return 1
	}
	resp, err := enroll(chassisURL, secretValue, newPub, *label, *kind)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: enroll new key: %v\n", explainEnrollErr(err))
		return 1
	}

	// Swap the on-disk key. Move the old one aside; write the new one
	// in its place; rewrite meta.
	rotatedPath := fmt.Sprintf("%s.rotated-%d", keyPath, time.Now().Unix())
	if err := os.Rename(keyPath, rotatedPath); err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: stash old key: %v\n", err)
		return 1
	}
	if err := SavePrivateKey(keyPath, newPriv); err != nil {
		// Try to restore the old key so the user isn't left empty-handed.
		_ = os.Rename(rotatedPath, keyPath)
		fmt.Fprintf(stderr, "auth rotate-key: save new key: %v\n", err)
		return 1
	}
	if err := SaveMeta(metaPath, Meta{
		ActorID:    resp.ActorID,
		KeyID:      resp.KeyID,
		ChassisURL: chassisURL,
		Label:      *label,
		EnrolledAt: time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: save meta: %v\n", err)
		return 1
	}

	// Revoke the old key using its OWN signature (it's still valid at
	// this point — we haven't told the server to forget it yet).
	oldSigner, err := signer.NewFileKeySignerFromKey(oldMeta.KeyID, oldPriv)
	if err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: WARNING new key enrolled but couldn't construct old signer: %v\n", err)
		fmt.Fprintf(stderr, "  manually revoke %s when you can.\n", oldMeta.KeyID)
		fmt.Fprintf(stdout, "rotated: new %s %s (was %s)\n", resp.ActorID, resp.KeyID, oldMeta.KeyID)
		fmt.Fprintf(stdout, "old key archived at %s\n", rotatedPath)
		return 0
	}
	revoker := client.New(client.Target{
		Addr: chassisURL,
		Auth: oldSigner,
	})
	if _, err := revoker.RevokeKey(context.Background(), oldMeta.KeyID); err != nil {
		fmt.Fprintf(stderr, "auth rotate-key: WARNING new key enrolled but revoking old key failed: %v\n", err)
		fmt.Fprintf(stderr, "  manually revoke %s when you can.\n", oldMeta.KeyID)
		// We still consider rotation successful — the new key is live.
	}

	fmt.Fprintf(stdout, "rotated: new %s %s (was %s)\n", resp.ActorID, resp.KeyID, oldMeta.KeyID)
	fmt.Fprintf(stdout, "old key archived at %s\n", rotatedPath)
	return 0
}
