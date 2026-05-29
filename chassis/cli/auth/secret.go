package auth

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// resolveSecret returns the dev-enroll secret, preferring the flag
// value when set. With no flag:
//
//   - stdin is a TTY → prompt with hidden input.
//   - stdin is a pipe → read one trimmed line.
//
// Keeping --secret means CI / scripts don't have to change; the prompt
// keeps the secret out of shell history and `ps` output.
func resolveSecret(flagValue string, stderr io.Writer) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(stderr, "enroll secret: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(stderr) // newline after the hidden input
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", errors.New("empty secret")
		}
		return s, nil
	}
	// Piped stdin.
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read secret from stdin: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return "", errors.New("empty secret on stdin")
	}
	return s, nil
}

// promptLine reads one trimmed line from stdin when stdin is a TTY,
// using `suggest` as the default value if the user just hits Enter.
// Returns (value, true, err) on a TTY; ("", false, nil) when stdin
// isn't a terminal so callers can fall back to non-interactive
// behaviour (typically: print an error and exit).
//
// Distinct from resolveSecret in that the input isn't hidden — these
// prompts are for non-sensitive choices like profile names.
func promptLine(stderr io.Writer, label, suggest string) (string, bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", false, nil
	}
	if suggest != "" {
		fmt.Fprintf(stderr, "%s [%s]: ", label, suggest)
	} else {
		fmt.Fprintf(stderr, "%s: ", label)
	}
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", true, fmt.Errorf("read input: %w", err)
	}
	s := strings.TrimSpace(line)
	if s == "" {
		return suggest, true, nil
	}
	return s, true, nil
}

// (promptYesNo lives in key_resolve.go — reused here to keep the
// y/N affordance consistent across the auth subcommands.)

// explainEnrollErr augments common server-side failures with hints
// the chassis itself can't include (the chassis answers from its
// state; the operator needs guidance about CLI/onboarding flows).
//
// Most importantly: a 404 from /auth/dev/enroll on the current
// chassis means EITHER the registry is non-empty (an admin already
// enrolled — auto-bootstrap burned) OR there's truly no dev secret
// configured. The far more common case is "someone already
// bootstrapped" — the hint leads with that and points at the
// invitation flow (which is what onboarding-after-first-admin is
// supposed to use anyway).
func explainEnrollErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "404") && strings.Contains(msg, "not_found") {
		return fmt.Errorf(
			"%w\n"+
				"  → the chassis isn't accepting bootstrap enrolments. Common reasons:\n"+
				"    • An admin has already enrolled (auto-bootstrap is single-use). To add another\n"+
				"      identity, have an existing admin run `txco auth invite` and then run\n"+
				"      `txco auth accept` on this side with the printed token.\n"+
				"    • OR the chassis has no --auth-dev-enroll-secret set AND no fresh-registry\n"+
				"      auto-bootstrap available. Restart with --auth-dev-enroll-secret=<s>\n"+
				"      (or TXCO_AUTH_DEV_ENROLL_SECRET=<s>) to enable a one-shot bootstrap.\n"+
				"  To replace your current enrolment with a different key instead, see\n"+
				"  `txco auth rotate-key`.", err)
	}
	if strings.Contains(msg, "401") && strings.Contains(msg, "invalid_enrollment_secret") {
		return fmt.Errorf("%w\n  → the secret you typed doesn't match the chassis's current bootstrap secret. "+
			"If the chassis just restarted, grab the fresh secret from its startup WARN; "+
			"if you set --auth-dev-enroll-secret explicitly, double-check the value.", err)
	}
	return err
}
