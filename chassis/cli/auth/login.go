package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runLogin mints a browser-auth bootstrap token and either opens the
// resulting URL or prints it. Pairs with the chassis's
// /auth/browser/{bootstrap,exchange,session} endpoints.
//
// `login` is distinct from `bootstrap-local`: the latter enrols the
// long-lived signing key against the chassis (one-time setup); this
// verb mints a short-lived browser session *derived* from that key.
// If `login` runs before any key is enrolled, the signed bootstrap
// call fails and we surface a hint to run `bootstrap-local` first.
func runLogin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "chassis admin endpoint (defaults to the meta file's chassis_url, or http://localhost:8081)")
	tenant := fs.String("tenant", "", "tenant slug to scope the session to (defaults to TXCO_TENANT, then the meta's default_tenant, then \"default\")")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	noOpen := fs.Bool("no-open", false, "print the URL instead of opening it in a browser")
	label := fs.String("label", "", "human-readable label for the session (shown in `auth sessions list`)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth login [flags]

Mint a single-use browser-login token by signing a request with the
current profile's key, then open the chassis admin UI at the
resulting URL. The browser exchanges the token for a session cookie;
from then on the admin UI can call protected admin endpoints in
signed-mode chassis.

`+"`login`"+` is distinct from `+"`bootstrap-local`"+`. Run
`+"`bootstrap-local`"+` once to enrol your signing key, then `+"`login`"+`
whenever you need a fresh browser session.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	applyTargetSelector(*targetSel, urlFlag, profile)
	resolvedProfile, err := ResolveProfile(*profile)
	if err != nil {
		PrintCLIErrorf(stderr, "auth login: %v", err)
		return 1
	}
	if resolvedProfile == ActiveNone {
		// "Logged out" profile + no signing key — bootstrap can't be
		// signed. Send the user back to `bootstrap-local`.
		PrintCLIError(stderr, "auth login: no signing key configured; run `txco auth bootstrap-local` first")
		return 1
	}

	target, err := buildSignedTarget(resolvedProfile, *urlFlag)
	if err != nil {
		PrintCLIErrorf(stderr, "auth login: %v", err)
		return 1
	}
	if target.Auth == nil {
		// buildSignedTarget falls through to unsigned when no key is
		// available — make that explicit so the user knows what to do.
		PrintCLIError(stderr, "auth login: no signing key configured; run `txco auth bootstrap-local` first")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	c := client.New(target)
	resp, err := c.BootstrapBrowserAuth(context.Background(), *label)
	if err != nil {
		// Most likely failure: the chassis can't find this key (key
		// not enrolled, or wrong tenant). Translate the typed error
		// to a useful hint where possible.
		var he *client.HTTPError
		if errors.As(err, &he) {
			switch he.StatusCode {
			case http.StatusUnauthorized:
				PrintCLIErrorf(stderr, "auth login: chassis rejected the signed request (%s)", he.Code)
				fmt.Fprintf(stderr, "  - if you haven't enrolled yet, run `txco auth bootstrap-local`\n")
				fmt.Fprintf(stderr, "  - if your key was revoked, re-enrol with `txco auth bootstrap-local` or `txco auth accept`\n")
			case http.StatusForbidden:
				PrintCLIErrorf(stderr, "auth login: forbidden (%s)", he.Code)
				fmt.Fprintf(stderr, "  this actor isn't a member of tenant %q; try `--tenant <other>`\n", target.Tenant)
			default:
				PrintCLIErrorf(stderr, "auth login: %v", err)
			}
			return 1
		}
		PrintCLIErrorf(stderr, "auth login: %v", err)
		return 1
	}

	fmt.Fprintln(stdout, loginIdentitySummary(resolvedProfile, target))

	if *noOpen {
		fmt.Fprintf(stdout, "Open this URL in your browser to sign in:\n\n  %s\n\n",
			resp.URL)
		return 0
	}
	if err := openBrowser(resp.URL); err != nil {
		// Fall back to printing — better than failing outright.
		PrintCLIErrorf(stderr, "auth login: couldn't open browser (%v); paste this URL instead:\n\n  %s", err, resp.URL)
		return 0
	}
	fmt.Fprintf(stdout, "Opened %s in your browser.\n",
		resp.URL)
	return 0
}

// loginIdentitySummary returns a one-line "who am I opening the UI as"
// summary, so the user can confirm the identity (and tenant + chassis)
// the browser session will carry before it pops open. Pulled from the
// local meta — no extra signed round-trip. Best-effort: if the meta
// can't be read it falls back to just the profile.
func loginIdentitySummary(profile string, target client.Target) string {
	who := ""
	if metaPath, err := MetaPath(profile); err == nil {
		if m, err := LoadMeta(metaPath); err == nil && m != nil {
			switch {
			case m.Label != "":
				who = m.Label
			case m.ActorID != "":
				who = m.ActorID
			}
		}
	}
	if who != "" {
		return fmt.Sprintf("Opening the admin UI as %s (profile %q, tenant %q) on %s",
			who, profile, target.Tenant, target.Addr)
	}
	return fmt.Sprintf("Opening the admin UI as profile %q (tenant %q) on %s",
		profile, target.Tenant, target.Addr)
}

// openBrowser shells out to the platform-native "open this URL"
// command. On macOS it's `open`; on Linux `xdg-open`; on Windows
// `cmd /c start`. The tradeoff vs a library: zero deps, works on the
// developer's machine, fails cleanly on headless boxes (caller falls
// back to printing).
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
