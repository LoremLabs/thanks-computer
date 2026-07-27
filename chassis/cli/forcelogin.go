package cli

import (
	"flag"
	"io"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/cloud"
)

// `txco ui --force-login` support.
//
// `txco ui cloud` is an alias for `auth login --profile cloud`, which signs a
// browser-session bootstrap with the long-lived ed25519 key on disk. That key
// never expires and is never re-verified against the identity provider, so the
// admin UI opens without a login prompt even right after signing out — the
// browser session and the chassis key are two separate credentials.
//
// --force-login runs the cloud OAuth handshake FIRST (`txco login`, which
// always opens the browser — it has no stored-token short-circuit), then falls
// through to the normal signed bootstrap. The service sends prompt=login on the
// authorize request, so the identity provider re-prompts rather than silently
// re-issuing a code.
//
// This lives in the `cli` package, not `auth`, because `cloud` imports `auth` —
// so `auth` cannot call back into `cloud`. `cli` imports both.

// takeForceLogin strips --force-login from args and reports whether it was set.
//
// Hand-rolled rather than done with a FlagSet because the remaining args are
// passed through verbatim to `auth login`, which parses them with its own
// FlagSet — this only has to recognise and remove one flag. Accepts every form
// Go's flag package would: one or two dashes, bare or `=<bool>`.
func takeForceLogin(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	force := false
	for _, a := range args {
		name, value, hasValue := strings.Cut(a, "=")
		switch strings.TrimLeft(name, "-") {
		case "force-login":
			// Only treat it as our flag if it actually had a leading dash;
			// a bare positional named "force-login" is a profile name.
			if !strings.HasPrefix(name, "-") {
				break
			}
			if hasValue {
				// A malformed value (`--force-login=nope`) is left for
				// auth login's FlagSet to reject rather than silently
				// swallowed here.
				b, err := strconv.ParseBool(value)
				if err != nil {
					out = append(out, a)
					continue
				}
				force = force || b
			} else {
				force = true
			}
			continue
		}
		out = append(out, a)
	}
	return out, force
}

// uiTargetProfile resolves which profile `txco ui <args>` will act as, using the
// same precedence as auth login: --profile, then --target (when it names a
// profile rather than a raw URL), then the trailing positional.
//
// It mirrors auth login's flag set exactly and parses with the stdlib rather
// than scanning by hand, so `--tenant foo` is correctly read as a flag value and
// not mistaken for the positional profile. A parse error yields "" — the real
// error is reported a moment later by auth.Dispatch, which parses the same args
// for real.
func uiTargetProfile(args []string) string {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	urlFlag := fs.String("url", "", "")
	profile := fs.String("profile", "", "")
	targetSel := fs.String("target", "", "")
	_ = fs.String("tenant", "", "")
	_ = fs.String("label", "", "")
	_ = fs.Bool("no-open", false, "")
	if err := fs.Parse(args); err != nil {
		return ""
	}
	if *targetSel == "" && fs.NArg() > 0 {
		*targetSel = fs.Arg(0)
	}
	// Folds --target into --url/--profile exactly as auth login does: a value
	// containing ":" is a raw URL, anything else names a profile.
	applyTargetSelector(*targetSel, urlFlag, profile)
	return strings.TrimSpace(*profile)
}

// applyTargetSelector mirrors auth.applyTargetSelector (unexported there).
// Duplicated rather than exported because it is three lines and the auth copy
// documents itself as the canonical one; keep the two in step.
func applyTargetSelector(targetSel string, url, profile *string) {
	t := strings.TrimSpace(targetSel)
	if t == "" {
		return
	}
	if strings.Contains(t, ":") { // raw admin URL, not a profile name
		if strings.TrimSpace(*url) == "" {
			*url = t
		}
		return
	}
	if strings.TrimSpace(*profile) == "" {
		*profile = t
	}
}

// isCloudProfile reports whether a profile name is one that cloud enrollment
// owns. Mirrors cloud.derivedCloudProfile's naming scheme: the prod cloud keeps
// the bare name "cloud"; every other cloud gets a "cloud-" prefix. An empty
// name means "no profile named", which cloud login resolves to its own default.
func isCloudProfile(p string) bool {
	return p == "" || p == "cloud" || strings.HasPrefix(p, "cloud-")
}

// forceCloudLogin runs the cloud OAuth sign-in for the profile `txco ui` is
// about to use. Returns 0 to continue on to the signed bootstrap.
func forceCloudLogin(args []string, stdout, stderr io.Writer) int {
	profile := uiTargetProfile(args)
	if !isCloudProfile(profile) {
		// A self-hosted chassis profile has no identity provider behind it —
		// its key came from `bootstrap-local`, not an OAuth enrollment. Running
		// cloud login here would sign in to the prod cloud and try to enroll
		// this profile against it, which is not what the user asked for.
		auth.PrintCLIErrorf(stderr,
			"ui: --force-login applies to cloud profiles only, and %q isn't one", profile)
		return 1
	}

	loginArgs := []string{"login"}
	if profile != "" {
		loginArgs = append(loginArgs, "--profile", profile)
	}
	// "cloud-dev" is cloud.derivedCloudProfile's name for the local dev cloud.
	// Without --dev, cloud login would default to the PROD cloud base and sign
	// the dev profile in against the wrong issuer.
	if profile == "cloud-dev" {
		loginArgs = append(loginArgs, "--dev")
	}
	return cloud.Dispatch(loginArgs, stdout, stderr)
}
