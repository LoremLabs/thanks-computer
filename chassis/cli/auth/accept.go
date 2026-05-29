package auth

import (
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runAccept is the invitee-side of the invitation flow. Unsigned —
// the token from --token (or stdin) is the only credential. Picks a
// signing key via resolveEnrollmentKey (ssh-agent preferred, then
// ~/.ssh/id_ed25519 on TTY, then fresh keygen), POSTs the public
// half to /auth/invitations/consume, and writes the resulting
// actor_id/key_id to meta so future signed commands just work.
func runAccept(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth accept", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultChassisURL, "chassis admin endpoint")
	token := fs.String("token", "", "invitation token (prompted on stdin if omitted)")
	homeHint := HomePathPretty()
	profile := fs.String("profile", "", fmt.Sprintf("profile name; meta lands at %s/keys/<profile>.meta.json", homeHint))
	name := fs.String("name", "", "alias for --profile (kept for back-compat; prompted on collision if both omitted)")
	label := fs.String("label", "", "label stored with your new actor")
	kind := fs.String("kind", "human", "actor kind written to the new actor row")
	sshAgent := fs.Bool("ssh-agent", false, "force ssh-agent backend (override auto-detect)")
	noSSHAgent := fs.Bool("no-ssh-agent", false, "skip ssh-agent even when reachable")
	sshKey := fs.String("ssh-key", "", "use an existing on-disk key (e.g. ~/.ssh/id_ed25519)")
	newKey := fs.Bool("new-key", false, fmt.Sprintf("generate a fresh key under %s (skip auto-detect)", homeHint))
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco auth accept [flags]

Redeem an invitation token to mint your own actor + key on the
chassis. After this command, 'txco apply', 'txco auth whoami', etc.
sign their requests automatically with the new key.

By default, ssh-agent is preferred when available; otherwise
~/.ssh/id_ed25519 is offered (TTY confirmation); otherwise a fresh
key is generated under %[1]s. --ssh-agent, --ssh-key, --new-key,
--no-ssh-agent steer the selection explicitly.

Flags:
`, homeHint)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// --profile is the modern spelling; --name is the back-compat
	// alias. Explicit --profile wins.
	if *profile != "" {
		*name = *profile
	}

	// Resolve the key choice first so we fail fast on filesystem
	// collisions or unreachable agents before asking the user to
	// paste a (possibly secret) token.
	resolveName := *name
	if resolveName == "" {
		resolveName = defaultKeyName
	}
	ek, err := resolveEnrollmentKey(EnrollmentChoices{
		SSHAgent:   *sshAgent,
		NoSSHAgent: *noSSHAgent,
		SSHKey:     *sshKey,
		NewKey:     *newKey,
		Name:       resolveName,
		Label:      *label,
	}, os.Stdin, term.IsTerminal(int(os.Stdin.Fd())), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "auth accept: %v\n", err)
		return 1
	}

	tok, err := resolveTokenInput(*token, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "auth accept: %v\n", err)
		return 2
	}

	// Default --label from the agent's key comment if the caller
	// didn't pass one — saves typing for the common "user@host"
	// case and makes /auth/actors listings legible.
	effLabel := *label
	if effLabel == "" {
		effLabel = ek.CommentSuggestion
	}
	resp, err := consume(*url, tok, ek.PublicKey, effLabel, *kind)
	if err != nil {
		ek.CleanupOnFailure()
		fmt.Fprintf(stderr, "auth accept: %v\n", err)
		return 1
	}

	if err := ek.PersistFreshKey(effLabel); err != nil {
		fmt.Fprintf(stderr, "auth accept: persist key: %v\n", err)
		return 1
	}

	// Meta name: if generateFreshKey had to rename (collision +
	// TTY prompt), use the user's chosen name. Otherwise the
	// caller-supplied name (or "local" default) is authoritative.
	metaName := resolveName
	if ek.MetaName() != "" {
		metaName = ek.MetaName()
	}
	metaPath, err := MetaPath(metaName)
	if err != nil {
		fmt.Fprintf(stderr, "auth accept: %v\n", err)
		return 1
	}
	defaultTenant := resp.TenantSlug
	if err := SaveMeta(metaPath, Meta{
		ActorID:       resp.ActorID,
		KeyID:         resp.KeyID,
		ChassisURL:    *url,
		Label:         effLabel,
		EnrolledAt:    time.Now().UTC(),
		KeySource:     ek.KeySource,
		PublicKeyB64:  PublicKeyB64(ek.PublicKey),
		KeyPath:       ek.KeyPath,
		DefaultTenant: defaultTenant,
	}); err != nil {
		fmt.Fprintf(stderr, "auth accept: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "enrolled %s %s capabilities=%v\n", resp.ActorID, resp.KeyID, resp.Capabilities)
	fmt.Fprintf(stdout, "fingerprint: %s\n", ek.Fingerprint)
	if ek.KeyPath != "" {
		fmt.Fprintf(stdout, "key:  %s\n", ek.KeyPath)
	} else {
		fmt.Fprintf(stdout, "key:  (ssh-agent)\n")
	}
	fmt.Fprintf(stdout, "meta: %s\n", metaPath)
	if defaultTenant != "" {
		fmt.Fprintf(stdout, "tenant: %s\n", defaultTenant)
	}
	return 0
}

// resolveTokenInput mirrors resolveSecret's UX: flag value if set,
// otherwise read from stdin (hidden if TTY, plain line if piped).
func resolveTokenInput(flagValue string, stderr io.Writer) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	// Reuse the password-style stdin prompt. Same shape and noise
	// posture as 'txco auth bootstrap-local --secret'.
	return resolveSecret("", stderr)
}

// suggestKeyName produces a sensible default for the rename prompt
// when the user's chosen key name collides. Uses the lowercased
// label if it's a usable name, otherwise picks the lowest free
// `local-N` (N≥2). Lives here (not in key_resolve.go) so the rename
// prompt logic stays grouped with the rest of the name-handling
// helpers it shares.
func suggestKeyName(label string) string {
	if s := sanitizeKeyName(label); s != "" && s != defaultKeyName {
		if kp, err := KeyPath(s); err == nil {
			if _, statErr := os.Stat(kp); os.IsNotExist(statErr) {
				return s
			}
		}
	}
	for i := 2; i < 1000; i++ {
		n := fmt.Sprintf("%s-%d", defaultKeyName, i)
		if kp, err := KeyPath(n); err == nil {
			if _, statErr := os.Stat(kp); os.IsNotExist(statErr) {
				return n
			}
		}
	}
	return defaultKeyName + "-N"
}

// sanitizeKeyName lowercases and strips a label down to the
// characters allowed in a key name. Returns "" if nothing usable
// is left after sanitisation.
func sanitizeKeyName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// validKeyName accepts the same character set we use elsewhere for
// on-disk paths: letters, digits, '_', '-'. Conservatively rejects
// anything that might surprise a shell or filesystem.
func validKeyName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// nextSuggestion bumps a "name-N" suggestion to "name-(N+1)". If the
// prior suggestion didn't end in a number, appends "-2". Keeps the
// rename prompt useful when the user is rapid-firing through a
// directory full of pre-existing keys.
func nextSuggestion(prev string) string {
	idx := strings.LastIndex(prev, "-")
	if idx > 0 && idx < len(prev)-1 {
		if n, err := strconv.Atoi(prev[idx+1:]); err == nil {
			return prev[:idx+1] + strconv.Itoa(n+1)
		}
	}
	return prev + "-2"
}

// consume is a small wrapper for runAccept and tests. Kept package-
// local so callers don't have to assemble client.ConsumeInvitationRequest
// by hand.
func consume(url, token string, pub ed25519.PublicKey, label, kind string) (*client.DevEnrollResponse, error) {
	c := client.New(client.Target{Addr: url})
	return c.ConsumeInvitation(context.Background(), client.ConsumeInvitationRequest{
		Token:        token,
		PublicKeyB64: PublicKeyB64(pub),
		Algorithm:    "ed25519",
		Label:        label,
		Kind:         kind,
	})
}
