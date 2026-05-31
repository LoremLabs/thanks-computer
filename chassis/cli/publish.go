package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/pflag"
	"oras.land/oras-go/v2"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// runPackagePublish validates a local package, builds a single-layer OCI
// artifact, pushes it to a registry, and prints the resolved digest.
//
//	txco package publish --to oci://ghcr.io/you/support-basic:0.1.0 [./dir]
func runPackagePublish(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package publish", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	to := fs.String("to", "", "destination OCI reference (oci://host/ns/name:tag)")
	dryRun := fs.Bool("dry-run", false, "validate + build the artifact; do not push")
	doSign := fs.Bool("sign", false, "sign the artifact with an ed25519 key after pushing")
	keyPath := fs.String("key", "", "signing key path (default: $TXCO_HOME/keys/signing.ed25519)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco package publish --to <oci-ref> [<dir>]

Validate the package at <dir> (default "."), pack it into a single-layer OCI
artifact, and push it. Auth comes from your docker config (or TXCO_OCI_*).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(stderr, "package publish: --to <oci-ref> is required")
		return 2
	}
	dir := fs.Arg(0)
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "package publish: %v\n", err)
		return 1
	}

	m, err := manifest.ParseFile(filepath.Join(abs, manifest.FileName))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "package publish: no %s at %s\n", manifest.FileName, abs)
		} else {
			fmt.Fprintf(stderr, "package publish: %v\n", err)
		}
		return 1
	}
	if probs := manifest.Validate(m, os.DirFS(abs), "."); len(probs) > 0 {
		fmt.Fprintf(stderr, "package publish: %s has %d problem%s; not publishing:\n", nameOr(m.Name, "package"), len(probs), pluralS(len(probs)))
		for _, p := range probs {
			fmt.Fprintf(stderr, "  %v\n", p)
		}
		return 1
	}

	ref, err := source.ParseRef(*to)
	if err != nil {
		fmt.Fprintf(stderr, "package publish: --to: %v\n", err)
		return 1
	}
	if ref.Digest != "" {
		fmt.Fprintln(stderr, "package publish: --to must be a tag (oci://…:tag), not a digest")
		return 1
	}

	if *dryRun {
		fmt.Fprintf(stdout, "package publish (dry-run): %s %s → %s (validated; not pushed)\n", m.Name, m.Version, ref.Reference())
		return 0
	}

	digest, err := source.Publish(context.Background(), abs, ref)
	if err != nil {
		fmt.Fprintf(stderr, "package publish: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "published %s\n", "oci://"+ref.WithDigest(digest))

	if *doSign {
		if code := signPublished(ref, digest, *keyPath, stdout, stderr); code != 0 {
			return code
		}
	}
	return 0
}

// signPublished signs an already-pushed package artifact and uploads the
// signature to the same repository (tag sha256-<hex>.sig). The package is
// already published, so a signing failure returns non-zero with a re-run hint
// rather than unwinding the push.
func signPublished(ref source.ParsedRef, digest, keyPath string, stdout, stderr io.Writer) int {
	if keyPath == "" {
		p, err := auth.KeyPath("signing")
		if err != nil {
			fmt.Fprintf(stderr, "package publish: %v\n", err)
			return 1
		}
		keyPath = p
	}
	priv, err := auth.LoadPrivateKey(keyPath)
	if err != nil {
		fmt.Fprintf(stderr, "package publish: load signing key: %v\n  (generate one with `txco package key generate`)\n", err)
		return 1
	}
	s := sign.NewSigner(priv)
	signedAt := time.Now().UTC().Format(time.RFC3339)
	err = source.PushSignature(context.Background(), ref, func(dst oras.Target) (string, error) {
		return sign.SignArtifact(context.Background(), dst, digest, ref.Repository(), "oci://"+ref.Reference(), signedAt, s)
	})
	if err != nil {
		fmt.Fprintf(stderr, "package publish: published, but signing failed: %v\n  (re-run `txco package publish … --sign` to sign)\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "signed by %s (%s)\n", s.KeyID, sign.DigestToSigTag(digest))
	return 0
}
