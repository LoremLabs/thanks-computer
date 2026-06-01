// Package cloud implements `txco login` — cloud identity for the
// thanks-computer cloud, via OAuth (Authorization Code + PKCE). It is
// deliberately distinct from `txco auth …`, which manages the ed25519
// signing keys that grant administrative authority over a self-hosted
// chassis: the OAuth token here represents the signed-in *user/account*,
// while the ed25519 key represents admin authority over a chassis.
//
// The CLI is a public OAuth client (no secret) that talks ONLY to the
// cloud, treating it as a generic OAuth front door: `txco login` opens the
// browser to the cloud's authorize endpoint, captures the redirect on a
// loopback listener, exchanges the code at the cloud's token endpoint, and
// stores the tokens under $TXCO_HOME/cloud/<profile>.json (0600). The cloud
// brokers whatever upstream identity provider it uses; the CLI neither
// knows nor hardcodes it. Login-only for now: key enrollment and
// hosted-stack creation are deliberate fast-follows.
//
// Reached two ways, both arriving here with the verb in args[0]:
//   - top-level `txco login` / `txco logout` (dispatched from cli.go)
//   - `txco cloud <login|logout|whoami>` namespace
package cloud

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

const (
	// defaultCloudURL is the hosted control plane. The CLI treats it as a
	// generic OAuth front door: it opens <cloud>/auth (which adds the
	// identity-provider specifics — client_id, scope, the upstream IdP — and
	// redirects there) and exchanges the code at <cloud>/token. No
	// identity-provider specifics are baked into the CLI; the cloud owns them.
	defaultCloudURL = "https://www.thanks.computer"
	// devCloudURL is the local www-thanks-computer dev server (--dev).
	devCloudURL = "http://localhost:4200"
)

// loopbackPorts is the ordered set of 127.0.0.1 callback ports the CLI
// tries, binding the first free one. These MUST match the loopback
// redirect_uris the cloud (and the upstream identity provider it brokers)
// accept — the redirect_uri is matched exactly, so an unregistered port is
// rejected at code exchange.
var loopbackPorts = []int{45454, 45455, 45456, 45457}

// Dispatch routes the cloud verb in args[0].
func Dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "login":
		return runLogin(args[1:], stdout, stderr)
	case "logout":
		return runLogout(args[1:], stdout, stderr)
	case "whoami", "status":
		return runWhoami(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "cloud: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco login [flags]            Sign in to the thanks-computer cloud
  txco logout [flags]           Sign out (delete stored cloud tokens)
  txco cloud login|logout|whoami

Cloud login authenticates your *user account* against the hosted
thanks-computer control plane (sign in with GitHub, Twitter, email…).
This is separate from `+"`txco auth`"+`, which manages the ed25519 signing
keys that grant administrative authority over a chassis.

Tokens are stored under `+auth.HomePathPretty()+`/cloud/<profile>.json (0600).

Run `+"`txco login --help`"+` for flags.
`)
}

// openBrowser shells out to the platform-native "open this URL" command
// (macOS `open`, Linux `xdg-open`, Windows `cmd /c start`). Zero deps;
// fails cleanly on headless boxes so the caller can fall back to printing
// the URL. Mirrors auth.openBrowser (unexported there).
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		cmd = exec.Command("xdg-open", u)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", u)
	default:
		return fmt.Errorf("don't know how to open a browser on %s", runtime.GOOS)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
