package auth

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runRevoke signs a POST /auth/keys/<id>/revoke with the configured
// key. Revoking your own current key effectively logs you out — the
// next signed call will fail. Useful before discarding a workstation.
func runRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth revoke-key", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	name := fs.String("name", defaultKeyName, "key name to authenticate with")
	keyID := fs.String("key-id", "", "key id to revoke (required)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth revoke-key --key-id <id> [flags]

Revoke a key by id. Signed with --name's key (which may or may not be
the same key being revoked).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyID == "" {
		fmt.Fprintln(stderr, "auth revoke-key: --key-id is required")
		return 2
	}

	target, err := buildSignedTarget(*name, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth revoke-key: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth revoke-key: no signing key configured; revoke requires authentication")
		return 1
	}

	resp, err := client.New(target).RevokeKey(context.Background(), *keyID)
	if err != nil {
		fmt.Fprintf(stderr, "auth revoke-key: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", resp.KeyID)
	return 0
}
