package auth

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runWhoami calls GET /auth/whoami with the current key's signature
// and prints the chassis's view. Useful to confirm signed-auth is
// wired correctly before trying `txco apply`.
func runWhoami(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "chassis admin endpoint (defaults to the meta file's chassis_url, or http://localhost:8081)")
	addrFlag := fs.String("addr", "", "alias of --url (the chassis admin endpoint, matching the workspace commands)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth whoami [flags]

Sign and send GET /auth/whoami; echo the actor + capabilities the
chassis sees. If no key is configured, the request is sent unsigned
and the chassis's response indicates how it identified you (or didn't).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --profile is preferred; --name is the legacy alias. If both
	// are set, --profile wins (it's the new spelling). If neither
	// is set, ResolveProfile walks env + active-file + default.
	profileFlag := *profile
	if profileFlag == "" {
		profileFlag = *name
	}
	// --addr is an alias of --url so the endpoint flag spells the same as the
	// workspace commands (esp. for the top-level `txco whoami`). --addr wins
	// when both are set.
	endpoint := *urlFlag
	if *addrFlag != "" {
		endpoint = *addrFlag
	}
	// --target is the canonical selector: a profile name → the profile, a raw
	// URL → the endpoint (each only when not already set explicitly).
	applyTargetSelector(*targetSel, &endpoint, &profileFlag)

	resolvedProfile, err := ResolveProfile(profileFlag)
	if err != nil {
		fmt.Fprintf(stderr, "auth whoami: %v\n", err)
		return 1
	}
	if resolvedProfile == ActiveNone {
		// Logged out: just call the chassis unsigned and let it
		// report source=open. Tells the user "you're currently
		// not signing" in the same shape as any other whoami.
		resolvedProfile = ""
	}

	target, err := buildSignedTarget(resolvedProfile, endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "auth whoami: %v\n", err)
		return 1
	}

	resp, err := client.New(target).Whoami(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "auth whoami: %v\n", err)
		return 1
	}

	// Show which profile + chassis this answer is for. Without these,
	// a `whoami` that succeeds against one profile/URL looks like it
	// contradicts an `apply --profile X` that 401s against another —
	// the two are simply talking to different chassis.
	profileDisplay := resolvedProfile
	if profileDisplay == "" {
		profileDisplay = "(none — unsigned)"
	}
	fmt.Fprintf(stdout, "profile: %s\n", profileDisplay)
	fmt.Fprintf(stdout, "chassis: %s\n", target.Addr)
	fmt.Fprintf(stdout, "source: %s\n", resp.Source)
	if resp.ActorID != "" {
		fmt.Fprintf(stdout, "actor_id: %s\n", resp.ActorID)
	}
	if resp.KeyID != "" {
		fmt.Fprintf(stdout, "key_id: %s\n", resp.KeyID)
	}
	if resp.Label != "" {
		fmt.Fprintf(stdout, "label: %s\n", resp.Label)
	}
	if resp.SuperAdmin {
		fmt.Fprintln(stdout, "super_admin: true")
	}
	if len(resp.Capabilities) > 0 {
		fmt.Fprintf(stdout, "capabilities: %s\n", strings.Join(resp.Capabilities, ","))
	}
	// Memberships block: one indented row per tenant. Active tenant
	// (per ResolveTenant precedence) is marked with *. Renders only
	// when the server returned a memberships array — older chassis
	// produce no block at all.
	if len(resp.Memberships) > 0 {
		active := ResolveTenant("", profileFlag)
		fmt.Fprintln(stdout, "memberships:")
		for _, m := range resp.Memberships {
			mark := " "
			if m.TenantSlug == active {
				mark = "*"
			}
			fmt.Fprintf(stdout, "  %s %-20s %s\n",
				mark, m.TenantSlug, strings.Join(m.Capabilities, ","))
		}
	}
	return 0
}

// buildSignedTarget composes a client.Target from the named meta
// file, dispatching to whichever signer backend (file / agent /
// future hardware) the meta says owns the key. Falls back to
// unsigned when no meta exists, so `auth whoami` against an
// open-mode chassis still works.
//
// Honors meta.ChassisURL when the caller didn't pass --url, so a
// developer with multiple chassis can run `txco auth whoami --name X`
// against any of them without remembering URLs.
func buildSignedTarget(name, urlOverride string) (client.Target, error) {
	url := strings.TrimSpace(urlOverride)

	// Empty name = "logged out" or "no profile configured". Send
	// unsigned and let the chassis report what it sees (open mode,
	// or 401 from a signed-only chassis — either way the user gets
	// a clear answer).
	if name == "" {
		if url == "" {
			url = defaultChassisURL
		}
		return client.Target{Addr: url}, nil
	}

	metaPath, err := MetaPath(name)
	if err != nil {
		return client.Target{}, err
	}
	var meta *Meta
	if m, err := LoadMeta(metaPath); err == nil {
		meta = m
	} else if !errors.Is(err, os.ErrNotExist) {
		return client.Target{}, err
	}
	if url == "" && meta != nil {
		url = meta.ChassisURL
	}
	if url == "" {
		url = defaultChassisURL
	}

	// No meta → no signing key configured for this name. Send
	// unsigned; the server's response shows what it sees.
	if meta == nil {
		return client.Target{Addr: url}, nil
	}

	// Keyless profile (no key_id) — e.g. the `dev` profile that `txco dev`
	// auto-registers for its local chassis. Against a LOCAL chassis that's
	// fine: the open dev chassis accepts unsigned admin requests, so we send
	// unsigned. Mirrors the deploy resolver (which already tolerates a keyless
	// profile) and the LocalChassis relaxation in the tenant/secret commands.
	// For a REMOTE chassis it's an error — prod requires a real key, and
	// silently sending unsigned would only earn a 401. Checked before
	// LoadSignerForMetaPath, which would otherwise reject "no key_id" outright.
	if meta.KeyID == "" {
		if LocalChassis(url) {
			return client.Target{Addr: url}, nil
		}
		return client.Target{}, fmt.Errorf("profile %q has no signing key; run `txco auth bootstrap-local` to enroll one for %s", name, url)
	}

	s, err := LoadSignerForMetaPath(metaPath)
	if err != nil {
		return client.Target{}, err
	}
	if s == nil {
		// Meta says "file" but the key file is gone — surface
		// that as a clear error rather than silently sending
		// unsigned.
		return client.Target{}, fmt.Errorf("meta %q has no usable signing key; re-run `txco auth bootstrap-local` or `txco auth accept`", metaPath)
	}
	return client.Target{Addr: url, Auth: s}, nil
}
