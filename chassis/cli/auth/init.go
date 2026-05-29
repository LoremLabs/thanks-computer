package auth

import (
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

const defaultKeyName = "local"

// runInit generates an ed25519 keypair and writes only the private
// PEM. No network call, no meta file — that's `enroll`'s job. Most
// developers want `bootstrap-local` instead; init is for the case
// where the chassis is reached through some other enrollment path.
func runInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	homeHint := HomePathPretty()
	name := fs.String("name", defaultKeyName, fmt.Sprintf("key name under %s/keys/ (without .ed25519 suffix)", homeHint))
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth init [--name NAME]

Generate a fresh ed25519 keypair under %[1]s/keys/<NAME>.ed25519.
Prints the base64 public key so you can paste it into an enrollment
flow elsewhere. Refuses to overwrite an existing key file.

Flags:
`, homeHint)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	keyPath, err := KeyPath(*name)
	if err != nil {
		fmt.Fprintf(stderr, "auth init: %v\n", err)
		return 1
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		fmt.Fprintf(stderr, "auth init: generate: %v\n", err)
		return 1
	}
	if err := SavePrivateKey(keyPath, priv); err != nil {
		fmt.Fprintf(stderr, "auth init: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "key: %s\n", keyPath)
	fmt.Fprintf(stdout, "public_key_b64: %s\n", PublicKeyB64(pub))
	return 0
}
