package cloud

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/update"
)

// runLogin performs the OAuth Authorization Code + PKCE handshake through the
// hosted cloud front door, stores the resulting tokens, and prints the
// signed-in identity. The CLI talks ONLY to <cloud>: it opens <cloud>/auth
// (which adds the identity-provider specifics — client_id, scope, the
// upstream IdP — and redirects there) and exchanges the code at <cloud>/token.
// Login-only: it does not enroll an ed25519 key or create a stack (deliberate
// fast-follows).
func runLogin(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "cloud profile name (defaults to TXCO_PROFILE, else \"cloud\"; not the active chassis profile)")
	cloudFlag := fs.String("cloud", "", "cloud base URL (default "+defaultCloudURL+")")
	dev := fs.Bool("dev", false, "use the local dev cloud ("+devCloudURL+")")
	noOpen := fs.Bool("no-open", false, "print the sign-in URL instead of opening a browser")
	insecure := fs.Bool("insecure", false, "skip TLS verification (local dev cloud only)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for the browser callback")
	// Chassis enrollment (runs automatically after sign-in unless --no-enroll).
	noEnroll := fs.Bool("no-enroll", false, "sign in only; skip creating/enrolling the hosted chassis tenant")
	chassisFlag := fs.String("chassis", "", "chassis admin BASE URL for enrollment (overrides discovery)")
	tenant := fs.String("tenant", "", "tenant slug to claim on first enroll (non-interactive)")
	yes := fs.Bool("yes", false, "accept the server's suggested tenant slug without prompting")
	sshAgent := fs.Bool("ssh-agent", false, "enroll an ssh-agent key instead of the default")
	_ = fs.Bool("no-ssh-agent", false, "(deprecated; no-op — the default no longer auto-detects ssh-agent)")
	sshKey := fs.String("ssh-key", "", "use an existing on-disk key (e.g. ~/.ssh/id_ed25519)")
	newKey := fs.Bool("new-key", false, "generate a fresh chassis key under $TXCO_HOME instead of ~/.ssh/")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco login [flags]

Sign in to the thanks-computer cloud. Opens your browser to the cloud
sign-in page (which brokers the upstream identity provider — GitHub,
Twitter, email...), captures the redirect on a loopback port, and stores
the resulting tokens under `+auth.HomePathPretty()+`/cloud/<profile>.json.

This is your cloud *account* identity — separate from `+"`txco auth`"+`,
which manages chassis signing keys.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	banner.PrintLogo(stdout)

	// Resolve the cloud base: --cloud wins; else --dev; else the prod default.
	cloudBase := resolveCloudBase(*cloudFlag, *dev)

	// --insecure is honored only for a local dev cloud.
	insecureTLS := false
	if *insecure {
		if !isLocalCloud(cloudBase) {
			auth.PrintCLIError(stderr, "login: --insecure is only allowed for a local dev cloud, not "+cloudBase)
			return 1
		}
		insecureTLS = true
	}

	// Resolve the cloud profile name. Cloud login is independent of the
	// chassis signing-key state, so fall back to "cloud" when no profile is
	// active rather than refusing.
	profile := cloudProfile(*profileFlag, cloudBase)

	hc := newHTTPClient(insecureTLS)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// The cloud is a generic OAuth front door. Discover its endpoints; with no
	// discovery doc this falls back to <cloud>/auth and <cloud>/token.
	cfg, _ := discover(ctx, hc, cloudBase)

	state, err := genState()
	if err != nil {
		auth.PrintCLIErrorf(stderr, "login: %v", err)
		return 1
	}
	verifier, challenge, err := genPKCE()
	if err != nil {
		auth.PrintCLIErrorf(stderr, "login: %v", err)
		return 1
	}

	ls, err := startLoopbackServer()
	if err != nil {
		auth.PrintCLIErrorf(stderr, "login: %v", err)
		return 1
	}
	defer ls.Close()

	redirectURI := ls.RedirectURI()
	authURL := buildAuthorizeURL(cfg, redirectURI, state, challenge)

	switch {
	case *noOpen:
		fmt.Fprintf(stdout, "Open this URL in your browser to sign in:\n\n  %s\n\n", authURL)
	case openBrowser(authURL) != nil:
		fmt.Fprintf(stdout, "Couldn't open a browser automatically. Open this URL to sign in:\n\n  %s\n\n", authURL)
	default:
		fmt.Fprintf(stdout, "Opening your browser to sign in...\nIf it doesn't open, visit:\n\n  %s\n\n", authURL)
	}

	res, err := ls.Wait(ctx)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "login: timed out after %s waiting for the browser callback", *timeout)
		return 1
	}
	if res.Err != nil {
		auth.PrintCLIErrorf(stderr, "login: sign-in failed (%s)", res.Err.Error())
		return 1
	}
	if stateMismatch(res.State, state) {
		auth.PrintCLIError(stderr, "login: state mismatch — possible CSRF; aborting")
		return 1
	}
	if res.Code == "" {
		auth.PrintCLIError(stderr, "login: no authorization code returned")
		return 1
	}

	obtainedAt := time.Now()
	tr, err := exchangeCode(ctx, hc, cfg, res.Code, verifier, redirectURI)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "login: token exchange failed: %v", err)
		return 1
	}

	// Identity comes from the id_token. When the cloud advertises a JWKS,
	// verify the signature (and exp) against it; otherwise (no discovery /
	// no jwks_uri) fall back to an unverified decode — the token was just
	// received over TLS from the cloud's token endpoint in this exchange.
	var sub, email string
	if cfg.JwksURI != "" {
		s, e, verr := verifyIDToken(ctx, hc, cfg.JwksURI, tr.IDToken)
		if verr != nil {
			auth.PrintCLIErrorf(stderr, "login: id_token verification failed: %v", verr)
			return 1
		}
		sub, email = s, e
	} else {
		sub, email = claimsFromIDToken(tr.IDToken)
	}

	tok := CloudToken{
		Kind:         "cloud",
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
		ObtainedAt:   obtainedAt,
		Subject:      sub,
		Email:        email,
		Issuer:       cloudBase,
		CloudURL:     cloudBase,
	}
	if tr.ExpiresIn > 0 {
		tok.Expiry = obtainedAt.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	if err := SaveCloudToken(profile, tok); err != nil {
		auth.PrintCLIErrorf(stderr, "login: store token: %v", err)
		return 1
	}

	who := sub
	if who == "" {
		who = "(unknown identity)"
	}
	fmt.Fprintf(stdout, "Signed in as %s\n", who)
	fmt.Fprintf(stdout, "  profile: %s\n", profile)

	// After all the success paths below settle the bound chassis, warn
	// (warn-only) if this CLI is below the chassis's advertised minimum.
	// Deferred so it sees the profile's chassis_url written by performEnroll.
	defer warnClientOutdated(stdout, profile)

	// Onboarding: enroll a chassis key (creating the hosted tenant on first
	// run) so chassis commands target the cloud. Login already succeeded —
	// any enrollment failure degrades softly (the token stays saved).
	if *noEnroll {
		fmt.Fprintf(stdout, "\nSkipped chassis enrollment (--no-enroll). Run `txco cloud enroll` when ready.\n")
		return 0
	}
	endpoint := resolveEnrollEndpoint(cfg, *chassisFlag, *dev)
	if m, ok := alreadyEnrolled(profile); ok {
		if sameChassisHost(m.ChassisURL, endpoint) {
			fmt.Fprintf(stdout, "\nAlready enrolled — profile %q targets %s.\n", profile, m.ChassisURL)
			return 0
		}
		// The profile name is bound to a DIFFERENT chassis (e.g. a local
		// `bootstrap-local` dev chassis). Login succeeded, but don't clobber
		// that profile — enroll the cloud under its own name instead.
		fmt.Fprintf(stdout, "\nSigned in, but profile %q is already bound to a different chassis (%s),\n"+
			"so the cloud tenant was not enrolled. Rerun with `--profile <name>` to enroll\n"+
			"the cloud under a separate profile.\n", profile, m.ChassisURL)
		return 0
	}

	ec := enrollChoices{
		tenant:    *tenant,
		assumeYes: *yes,
		sshAgent:  *sshAgent,
		sshKey:    *sshKey,
		newKey:    *newKey,
	}
	if _, err := performEnroll(endpoint, tr.IDToken, profile, ec, stdout, stderr); err != nil {
		// Login succeeded; surface the partial failure (with the endpoint it
		// tried) as a red warning on stderr rather than a silent stdout note.
		auth.PrintCLIError(stderr, enrollDegradeMessage(err, endpoint))
	}
	return 0
}

// warnClientOutdated fetches the bound chassis's client-version policy and
// prints a warning (warn-only) when this CLI is below its advertised minimum.
// Entirely best-effort: no version/profile/URL, an unreachable chassis, or a
// chassis advertising no policy all result in silence.
func warnClientOutdated(stdout io.Writer, profile string) {
	if ClientVersion == "" {
		return
	}
	base := auth.ProfileChassisURL(profile)
	if base == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := update.FetchServerInfo(ctx, base, "txco-cli/"+ClientVersion)
	if err != nil {
		return
	}
	if n := update.OutdatedNotice(ClientVersion, info.Client); n != "" {
		fmt.Fprintf(stdout, "\n%s\n", n)
	}
}

// isLocalCloud reports whether the cloud base points at a loopback host (used
// to gate --insecure to local dev only).
func isLocalCloud(cloud string) bool {
	u, err := url.Parse(cloud)
	if err != nil {
		return false
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}
