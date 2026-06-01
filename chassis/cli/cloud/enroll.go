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
	tenant     string
	assumeYes  bool
	sshAgent   bool
	noSSHAgent bool
	sshKey     string
	newKey     bool
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
		Profile:     profile,
		TenantSlug:  ec.tenant,
		AssumeYes:   ec.assumeYes,
		SSHAgent:    ec.sshAgent,
		NoSSHAgent:  ec.noSSHAgent,
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

// enrollDegradeMessage renders the soft-failure line printed after a successful
// login when enrollment didn't complete. The endpoint-unavailable cases get the
// canonical wording; everything else surfaces the concrete reason.
func enrollDegradeMessage(err error) string {
	var he *client.HTTPError
	switch {
	case errors.As(err, &he) && he.StatusCode == http.StatusNotFound:
		return "Signed in, but no hosted chassis tenant was created (enrollment endpoint unavailable)."
	case isConnRefused(err):
		return "Signed in, but no hosted chassis tenant was created (enrollment endpoint unreachable)."
	default:
		return fmt.Sprintf("Signed in, but enrollment did not complete: %v", err)
	}
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
	sshAgent := fs.Bool("ssh-agent", false, "force ssh-agent backend for the chassis key")
	noSSHAgent := fs.Bool("no-ssh-agent", false, "skip ssh-agent even when reachable")
	sshKey := fs.String("ssh-key", "", "use an existing on-disk key (e.g. ~/.ssh/id_ed25519)")
	newKey := fs.Bool("new-key", false, "generate a fresh chassis key")
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
		tenant:     *tenant,
		assumeYes:  *yes,
		sshAgent:   *sshAgent,
		noSSHAgent: *noSSHAgent,
		sshKey:     *sshKey,
		newKey:     *newKey,
	}
	if _, err := performEnroll(endpoint, tok.IDToken, profile, ec, stdout, stderr); err != nil {
		auth.PrintCLIErrorf(stderr, "cloud enroll: %v", err)
		return 1
	}
	return 0
}
