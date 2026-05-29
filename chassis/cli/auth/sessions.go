package auth

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runSessions dispatches `txco auth sessions <verb>` to the right
// implementation. Currently `list` and `revoke <id>` — symmetric
// admin actions to the browser side's session lifecycle.
func runSessions(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "auth sessions: missing subcommand (list|revoke)")
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return runSessionsList(args[1:], stdout, stderr)
	case "revoke", "rm":
		return runSessionsRevoke(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printSessionsUsage(stdout)
		return 0
	default:
		PrintCLIErrorf(stderr, "auth sessions: unknown subcommand %q", args[0])
		printSessionsUsage(stderr)
		return 2
	}
}

func printSessionsUsage(w io.Writer) {
	fmt.Fprint(w, `
Usage: txco auth sessions <list|revoke> [flags]

Manage browser sessions on a chassis.

  list                  List active + historic sessions for the target tenant
  revoke <session_id>   Revoke a session by id (idempotent)

Both forms scope to the target tenant. Sessions are issued by
`+"`txco auth login`"+` and consumed by the admin UI.
`)
}

func runSessionsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth sessions list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	tenant := fs.String("tenant", "", "tenant slug")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco auth sessions list [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	target, err := resolveSignedTenantTarget(*profile, *urlFlag, *tenant)
	if err != nil {
		PrintCLIErrorf(stderr, "auth sessions list: %v", err)
		return 1
	}
	sessions, err := client.New(target).ListBrowserSessions(context.Background())
	if err != nil {
		PrintCLIErrorf(stderr, "auth sessions list: %v", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "no sessions")
		return 0
	}
	for _, s := range sessions {
		marker := "● "
		if s.RevokedAt != nil {
			marker = "✗ "
		}
		ua := s.UA
		if len(ua) > 40 {
			ua = ua[:37] + "..."
		}
		fmt.Fprintf(stdout, "%s%s  actor=%s  created=%s  last_seen=%s  ip=%s  ua=%q\n",
			marker, s.SessionID, s.ActorID, s.CreatedAt, s.LastSeenAt, s.IP, ua)
	}
	return 0
}

func runSessionsRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth sessions revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	tenant := fs.String("tenant", "", "tenant slug")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco auth sessions revoke <session_id> [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth sessions revoke: missing <session_id>")
		return 2
	}
	sessionID := strings.TrimSpace(fs.Arg(0))
	if sessionID == "" {
		PrintCLIError(stderr, "auth sessions revoke: empty <session_id>")
		return 2
	}

	target, err := resolveSignedTenantTarget(*profile, *urlFlag, *tenant)
	if err != nil {
		PrintCLIErrorf(stderr, "auth sessions revoke: %v", err)
		return 1
	}
	if err := client.New(target).RevokeBrowserSession(context.Background(), sessionID); err != nil {
		PrintCLIErrorf(stderr, "auth sessions revoke: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", sessionID)
	return 0
}

// resolveSignedTenantTarget is the shared "build a signed client.Target
// scoped to a tenant" helper for both sessions subcommands. Splits
// out so the verb bodies stay tight.
func resolveSignedTenantTarget(profileFlag, urlFlag, tenantFlag string) (client.Target, error) {
	resolvedProfile, err := ResolveProfile(profileFlag)
	if err != nil {
		return client.Target{}, err
	}
	if resolvedProfile == ActiveNone {
		return client.Target{}, fmt.Errorf("no signing key configured; run `txco auth bootstrap-local` first")
	}
	target, err := buildSignedTarget(resolvedProfile, urlFlag)
	if err != nil {
		return client.Target{}, err
	}
	target.Tenant = ResolveTenant(tenantFlag, resolvedProfile)
	return target, nil
}
