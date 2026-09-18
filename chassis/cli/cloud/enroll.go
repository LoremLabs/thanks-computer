package cloud

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// enrollChoices steers the ed25519 key selection + tenant naming for an enroll
// attempt. Mirrors the relevant subset of `txco auth accept`'s flags.
type enrollChoices struct {
	tenant    string
	assumeYes bool
	sshAgent  bool
	sshKey    string
	newKey    bool
	identity  string // who the id_token says is signing in; shown before "name your space"
}

// resolveEnrollEndpoint picks the FULL enroll URL: the cloud's advertised
// txco_enroll_endpoint (verbatim) → a --chassis BASE (+/auth/oauth/enroll) →
// the built-in constant (dev or prod). The CLI never guesses the path.
func resolveEnrollEndpoint(cfg *oidcConfig, chassisBase string, dev bool) string {
	if cfg != nil {
		if e := strings.TrimSpace(cfg.EnrollEndpoint); e != "" {
			return e
		}
	}
	if b := strings.TrimSpace(chassisBase); b != "" {
		return strings.TrimRight(b, "/") + enrollPath
	}
	if dev {
		return devEnrollEndpoint
	}
	return defaultEnrollEndpoint
}

// sameChassisHost reports whether an existing profile's chassis URL points at
// the same host:port as the resolved enroll endpoint — a genuine "already
// enrolled in this cloud chassis", as opposed to a name collision with a
// different (e.g. local dev) chassis profile.
func sameChassisHost(metaURL, endpointURL string) bool {
	mu, err1 := url.Parse(metaURL)
	eu, err2 := url.Parse(endpointURL)
	if err1 != nil || err2 != nil || mu.Host == "" {
		return false
	}
	return strings.EqualFold(mu.Host, eu.Host)
}

// alreadyEnrolled reports whether a usable chassis profile already exists for
// the given profile name (idempotency: a second `txco login` re-auths but
// skips enrollment). Returns the loaded Meta when present.
func alreadyEnrolled(profile string) (*auth.Meta, bool) {
	p, err := auth.MetaPath(profile)
	if err != nil {
		return nil, false
	}
	m, err := auth.LoadMeta(p)
	if err != nil || m == nil {
		return nil, false
	}
	if m.ChassisURL == "" || m.ActorID == "" {
		return nil, false
	}
	return m, true
}

// performEnroll runs the key enroll against endpoint using idToken and prints a
// success summary. The error (if any) is returned for the caller to render —
// during login it degrades softly; for `txco cloud enroll` it's a hard failure.
func performEnroll(endpoint, idToken, profile string, ec enrollChoices, stdout, stderr io.Writer) (*auth.OAuthEnrollResult, error) {
	res, err := auth.OAuthEnroll(auth.OAuthEnrollOptions{
		EndpointURL: endpoint,
		IDToken:     idToken,
		Identity:    ec.identity,
		Profile:     profile,
		TenantSlug:  ec.tenant,
		AssumeYes:   ec.assumeYes,
		SSHAgent:    ec.sshAgent,
		SSHKey:      ec.sshKey,
		NewKey:      ec.newKey,
		Stderr:      stderr,
	})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(stdout, "\nEnrolled in the cloud chassis:\n")
	fmt.Fprintf(stdout, "  tenant:  %s\n", res.TenantSlug)
	fmt.Fprintf(stdout, "  actor:   %s\n", res.ActorID)
	fmt.Fprintf(stdout, "  chassis: %s\n", res.ChassisURL)
	fmt.Fprintf(stdout, "  profile: %s (now active)\n", res.Profile)
	fmt.Fprintf(stdout, "\n`txco status` and `txco apply` now target your cloud tenant.\n")
	return res, nil
}

// enrollDegradeMessage renders the soft-failure block printed after a successful
// login when enrollment didn't complete. It always names the endpoint URL the
// CLI tried so the user can see where it was POSTing (e.g. prod vs a local
// --chassis). 404 / unreachable get specific wording; everything else surfaces
// the concrete error.
func enrollDegradeMessage(err error, endpoint string) string {
	var he *client.HTTPError
	var reason string
	switch {
	case errors.As(err, &he) && he.StatusCode == http.StatusNotFound:
		reason = "Signed in, but no hosted chassis tenant was created — the enrollment endpoint returned 404 (not enabled on that chassis)."
	case isConnRefused(err):
		reason = "Signed in, but no hosted chassis tenant was created — the enrollment endpoint is unreachable."
	default:
		reason = fmt.Sprintf("Signed in, but enrollment did not complete: %v", err)
	}
	return fmt.Sprintf("%s\n  endpoint: %s\nRun `txco cloud enroll` to try again.", reason, endpoint)
}

// isConnRefused heuristically detects a transport-layer failure (no HTTP
// response) so login can show the "unreachable" wording.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "no route to host") ||
		strings.Contains(msg, "timeout")
}

// runEnroll is `txco cloud enroll` — a standalone re-run of the chassis
// enrollment using the cloud token already stored for the profile (e.g. a
// second machine, or after a transient enroll failure during login).
func runEnroll(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cloud enroll", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "cloud profile to enroll (defaults to the active/\"cloud\" profile)")
	chassis := fs.String("chassis", "", "chassis admin BASE URL (overrides discovery; /auth/oauth/enroll is appended)")
	dev := fs.Bool("dev", false, "use the local dev cloud + chassis ("+devCloudURL+" / "+devEnrollEndpoint+")")
	tenant := fs.String("tenant", "", "tenant slug to claim on first enroll (non-interactive)")
	yes := fs.Bool("yes", false, "accept the server's suggested tenant slug without prompting")
	insecure := fs.Bool("insecure", false, "skip TLS verification (local dev cloud only)")
	sshAgent := fs.Bool("ssh-agent", false, "enroll an ssh-agent key instead of the default")
	_ = fs.Bool("no-ssh-agent", false, "(deprecated; no-op — the default no longer auto-detects ssh-agent)")
	sshKey := fs.String("ssh-key", "", "use an existing on-disk key (e.g. ~/.ssh/id_ed25519)")
	newKey := fs.Bool("new-key", false, "generate a fresh chassis key under $TXCO_HOME instead of ~/.ssh/")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco cloud enroll [flags]

Enroll (or re-enroll) a chassis key for an already-signed-in cloud profile,
creating your hosted tenant on first run and writing+activating the chassis
profile. Run `+"`txco login`"+` first.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	profile := resolveCloudReadProfile(*profileFlag)

	tok, err := LoadCloudToken(profile)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "cloud enroll: no cloud session for profile %q — run `txco login` first", profile)
		return 1
	}
	if tok.IDToken == "" {
		auth.PrintCLIError(stderr, "cloud enroll: stored session has no id_token — run `txco login` again")
		return 1
	}
	if tok.Expired(time.Now()) {
		auth.PrintCLIError(stderr, "cloud enroll: your cloud session has expired — run `txco login` again")
		return 1
	}

	cloudBase := tok.CloudURL
	if cloudBase == "" {
		cloudBase = tok.Issuer
	}

	insecureTLS := false
	if *insecure {
		if !isLocalCloud(cloudBase) {
			auth.PrintCLIError(stderr, "cloud enroll: --insecure is only allowed for a local dev cloud")
			return 1
		}
		insecureTLS = true
	}

	hc := newHTTPClient(insecureTLS)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, _ := discover(ctx, hc, cloudBase)
	endpoint := resolveEnrollEndpoint(cfg, *chassis, *dev)

	ec := enrollChoices{
		tenant:    *tenant,
		assumeYes: *yes,
		sshAgent:  *sshAgent,
		sshKey:    *sshKey,
		newKey:    *newKey,
		identity:  tok.Subject,
	}
	// Re-enrolling a profile means re-enrolling ITS key. Without this the
	// default key (~/.ssh/id_ed25519-txco) would be presented instead — a
	// different key, so a different actor — and the profile would be rebound
	// to it, leaving the actor the user was trying to repair untouched.
	if m, ok := alreadyEnrolled(profile); ok && sameChassisHost(m.ChassisURL, endpoint) &&
		!ec.sshAgent && ec.sshKey == "" && !ec.newKey {
		ec.sshKey, ec.sshAgent = auth.ExistingKeyChoice(m, profile)
		fmt.Fprintf(stderr, "re-enrolling profile %q with the key it already has\n", profile)
	}
	if _, err := performEnroll(endpoint, tok.IDToken, profile, ec, stdout, stderr); err != nil {
		auth.PrintCLIErrorf(stderr, "cloud enroll: %v\n  endpoint: %s", err, endpoint)
		return 1
	}
	return 0
}

// signBackIn is login's step for a profile that is already enrolled on this
// chassis: present the fresh id_token with the profile's own key, which lifts
// the "signed out in the admin UI — sign in again" marker. Returns a non-zero
// exit code only when the user picked the wrong account; a chassis that cannot
// be reached degrades to a warning, because the cloud sign-in itself worked.
func signBackIn(endpoint, idToken, profile, signedInAs string, m *auth.Meta, stderr io.Writer) (*auth.OAuthReauthResult, int) {
	res, err := auth.OAuthReauth(auth.OAuthReauthOptions{
		EnrollEndpointURL: endpoint,
		IDToken:           idToken,
		Profile:           profile,
	})
	if err == nil {
		return res, 0
	}
	var mismatch *auth.ReauthMismatchError
	if errors.As(err, &mismatch) {
		who := mismatch.SignedInAs
		if who == "" {
			who = signedInAs
		}
		auth.PrintCLIErrorf(stderr, "login: you signed in as %s, but profile %q was enrolled with a different account", who, profile)
		fmt.Fprintf(stderr, "  this machine's key belongs to %s on %s\n", spaceName(m.DefaultTenant), m.ChassisURL)
		fmt.Fprintf(stderr, "  nothing was changed — run `txco login --profile %s` again and choose the account that created it\n", profile)
		return nil, 1
	}
	auth.PrintCLIErrorf(stderr, "login: signed in, but couldn't confirm it with the chassis: %v", err)
	fmt.Fprintf(stderr, "  if `txco ui` still says you are signed out, run `txco login --profile %s` again\n", profile)
	return nil, 0
}

// spaceName renders a tenant slug for a sentence, tolerating an unknown one.
func spaceName(slug string) string {
	if strings.TrimSpace(slug) == "" {
		return "its cloud space"
	}
	return fmt.Sprintf("the %q space", slug)
}
