package auth

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"golang.org/x/term"
)

// LocalChassis reports whether a chassis URL points at the local machine
// (localhost / loopback, including the `*.localhost` dev hostnames). A
// mutating command against a local chassis never prompts — the whole point
// of the confirm-guard is to catch a write aimed at a *remote* chassis
// (prod/staging) that the operator may not have meant to hit.
func LocalChassis(chassisURL string) bool {
	h := hostOf(chassisURL)
	switch h {
	case "", "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return strings.HasSuffix(h, ".localhost")
}

// hostOf extracts the lowercased hostname from a chassis URL, tolerating a
// missing scheme (e.g. "127.0.0.1:8081"). Returns "" when unparseable —
// treated as local by LocalChassis (a blank/garbage endpoint can't be a
// remote we'd want to guard).
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// StdinIsTTY reports whether os.Stdin is an interactive terminal. Commands
// pass the result (ANDed with "not --json") as ConfirmTarget's `interactive`
// argument.
func StdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// ConfirmTargetStd is ConfirmTarget wired to os.Stdin and the real terminal
// check — the shape the auth subcommands use. jsonOut suppresses the prompt
// (machine output stays clean → fail-closed unless --yes).
func ConfirmTargetStd(name, chassisURL string, assumeYes, jsonOut bool, stderr io.Writer) error {
	return ConfirmTarget(name, chassisURL, assumeYes, StdinIsTTY() && !jsonOut, os.Stdin, stderr)
}

// ConfirmTarget announces the resolved target and, for a MUTATING command
// against a non-local chassis, requires confirmation before the command
// proceeds. Call it from mutating commands only — after the target resolves,
// before the first write. Read-only commands don't call it. Returns a
// non-nil error to abort the command.
//
//   - Always prints "→ <name> (<url>)" so the operator sees what they're
//     about to change (generalizes the per-command target echo apply used).
//   - Local chassis (localhost / loopback / *.localhost): never prompts.
//   - assumeYes (--yes): skips the prompt (the CI / scripted path).
//   - interactive terminal: prompts "proceed? [y/N]" (defaults to no).
//   - non-interactive without --yes: FAILS CLOSED, telling the caller to
//     pass --yes — so a piped/CI invocation never silently mutates a remote
//     chassis.
func ConfirmTarget(name, chassisURL string, assumeYes, interactive bool, stdin io.Reader, stderr io.Writer) error {
	return ConfirmTargetT(name, chassisURL, "", assumeYes, interactive, stdin, stderr)
}

// ConfirmTargetT is ConfirmTarget plus the tenant the command will write to,
// surfaced in the banner as "→ name (url, tenant T)" so the operator sees BOTH
// the chassis and the tenant before a remote write. tenant "" omits the clause
// (and keeps ConfirmTarget's output byte-for-byte). Same prompt/fail-closed
// semantics as ConfirmTarget.
func ConfirmTargetT(name, chassisURL, tenant string, assumeYes, interactive bool, stdin io.Reader, stderr io.Writer) error {
	endpoint := chassisURL
	if tenant != "" {
		endpoint = fmt.Sprintf("%s, tenant %s", chassisURL, tenant)
	}
	label := endpoint
	if name != "" {
		label = fmt.Sprintf("%s (%s)", name, endpoint)
	}
	fmt.Fprintf(stderr, "→ %s\n", label)

	if LocalChassis(chassisURL) || assumeYes {
		return nil
	}
	if !interactive {
		return fmt.Errorf("refusing to modify non-local chassis %s without confirmation; re-run with --yes", chassisURL)
	}
	if !promptYesNo(stdin, stderr, "proceed? [y/N]: ", false) {
		return fmt.Errorf("aborted")
	}
	return nil
}
