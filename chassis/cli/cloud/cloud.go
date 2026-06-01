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
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

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

	// defaultEnrollEndpoint is the hosted chassis admin enroll endpoint — a
	// FULL URL (the CLI never guesses the path). Fallback when the cloud's
	// discovery doc advertises no txco_enroll_endpoint and no --chassis is set.
	defaultEnrollEndpoint = "https://admin.thanks.computer/auth/oauth/enroll"
	// devEnrollEndpoint is the local chassis admin enroll endpoint (--dev).
	devEnrollEndpoint = "http://localhost:8081/auth/oauth/enroll"
	// enrollPath is appended to a --chassis BASE URL to form the full endpoint.
	enrollPath = "/auth/oauth/enroll"
)

// loopbackPorts is the ordered set of 127.0.0.1 callback ports the CLI
// tries, binding the first free one. These MUST match the loopback
// redirect_uris the cloud (and the upstream identity provider it brokers)
// accept — the redirect_uri is matched exactly, so an unregistered port is
// rejected at code exchange.
var loopbackPorts = []int{45454, 45455, 45456, 45457}

// resolveCloudBase picks the cloud base URL from --cloud / --dev, defaulting to
// the prod cloud. Shared so login and the profile-derivation agree.
func resolveCloudBase(cloudFlag string, dev bool) string {
	if b := strings.TrimSpace(cloudFlag); b != "" {
		return b
	}
	if dev {
		return devCloudURL
	}
	return defaultCloudURL
}

// derivedCloudProfile is the default profile name for a cloud target. The name
// is USER-VISIBLE — enrollment makes it the active chassis profile shown by
// `txco status` / `txco whoami` — so the canonical prod cloud keeps the clean
// bare name "cloud". Other targets get a suffix so distinct clouds never share a
// token/enrollment: --dev → "cloud-dev"; a custom --cloud → "cloud-<host>".
func derivedCloudProfile(cloudBase string) string {
	base := strings.TrimRight(strings.TrimSpace(cloudBase), "/")
	switch base {
	case "", strings.TrimRight(defaultCloudURL, "/"):
		return "cloud"
	case strings.TrimRight(devCloudURL, "/"):
		return "cloud-dev"
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "cloud-custom"
	}
	r := strings.NewReplacer(":", "-", ".", "-")
	return "cloud-" + r.Replace(strings.ToLower(u.Host))
}

// cloudProfile resolves the profile name for a WRITE command (login): explicit
// --profile → TXCO_PROFILE → the cloud-derived default. It deliberately does NOT
// inherit the chassis *active-profile file* — doing so let a leftover local
// chassis profile (e.g. a `bootstrap-local` "tmp" at localhost) capture
// `txco login` and enroll against the wrong chassis.
func cloudProfile(flagValue, cloudBase string) string {
	if p := strings.TrimSpace(flagValue); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("TXCO_PROFILE")); p != "" && p != auth.ActiveNone {
		return p
	}
	return derivedCloudProfile(cloudBase)
}

// resolveCloudReadProfile resolves the profile for a READ/resume command
// (whoami / logout / enroll), which acts on an EXISTING cloud session: explicit
// --profile → TXCO_PROFILE → the active profile if it has a cloud token → the
// sole cloud token if there's exactly one → the prod-derived default. This lets
// `txco whoami` "just work" after `txco login --dev` (which set the active
// profile) without re-specifying the cloud.
func resolveCloudReadProfile(flagValue string) string {
	if p := strings.TrimSpace(flagValue); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("TXCO_PROFILE")); p != "" && p != auth.ActiveNone {
		return p
	}
	if active, err := auth.ReadActiveProfile(); err == nil && active != "" && active != auth.ActiveNone {
		if cloudTokenExists(active) {
			return active
		}
	}
	if sole, ok := soleCloudProfile(); ok {
		return sole
	}
	return derivedCloudProfile(defaultCloudURL)
}

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
	case "enroll":
		return runEnroll(args[1:], stdout, stderr)
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
  txco cloud login|logout|whoami|enroll

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
