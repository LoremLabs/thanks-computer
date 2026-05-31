package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
)

// runPackageKey routes `txco package key <subcommand>`.
func runPackageKey(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: txco package key generate [--name <n>] [--out <dir>]")
		return 2
	}
	switch args[0] {
	case "generate", "gen":
		return runPackageKeyGenerate(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "Usage: txco package key generate [--name <n>] [--out <dir>]")
		return 0
	default:
		fmt.Fprintf(stderr, "package key: unknown subcommand %q\n", args[0])
		return 2
	}
}

// runPackageKeyGenerate creates an ed25519 package-signing keypair and prints
// the public key plus a ready-to-paste trust: snippet. Reuses the auth key
// helpers so signing keys live beside auth keys (under $TXCO_HOME/keys) and
// interoperate with ssh tooling.
func runPackageKeyGenerate(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package key generate", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "signing", "key name (file becomes <name>.ed25519)")
	out := fs.String("out", "", "output directory (default: $TXCO_HOME/keys)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco package key generate [--name <n>] [--out <dir>]

Generate an ed25519 package-signing keypair. Sign with `+"`txco package publish --sign`"+`,
then add the printed public key to a consumer's txco.yaml `+"`trust:`"+` block.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var keyPath string
	var err error
	if *out != "" {
		if err := os.MkdirAll(*out, 0o700); err != nil {
			fmt.Fprintf(stderr, "package key: %v\n", err)
			return 1
		}
		keyPath = filepath.Join(*out, *name+".ed25519")
	} else if keyPath, err = auth.KeyPath(*name); err != nil {
		fmt.Fprintf(stderr, "package key: %v\n", err)
		return 1
	}

	pub, priv, err := auth.GenerateKey()
	if err != nil {
		fmt.Fprintf(stderr, "package key: %v\n", err)
		return 1
	}
	if err := auth.SavePrivateKey(keyPath, priv); err != nil {
		fmt.Fprintf(stderr, "package key: %v\n", err)
		return 1
	}

	pubLine, _ := os.ReadFile(keyPath + ".pub")
	fmt.Fprintf(stdout, "generated signing key %s\n", sign.KeyIDForPub(pub))
	fmt.Fprintf(stdout, "  private: %s\n", keyPath)
	fmt.Fprintf(stdout, "  public:  %s.pub\n\n", keyPath)
	fmt.Fprintln(stdout, "Add this to a consumer's txco.yaml to trust packages you sign:")
	fmt.Fprintf(stdout, "\ntrust:\n  keys:\n    - name: %s\n      pubkey: \"%s\"\n", *name, strings.TrimSpace(string(pubLine)))
	return 0
}
