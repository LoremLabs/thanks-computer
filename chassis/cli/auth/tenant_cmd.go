package auth

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// resolveProfileForTenant walks the standard profile-resolution chain
// (--profile flag → --name legacy alias → TXCO_PROFILE env → the
// $TXCO_HOME/active marker → "local") and collapses ActiveNone back
// to "" so callers can pass the result straight to buildSignedTarget.
//
// Without this, every tenant verb that defaulted --name to literal
// "local" would sign with whatever's in $TXCO_HOME/keys/local.* —
// even when the user's *active* profile is something else. That
// produced silent "unknown_key" 401s when local.* held a stale key
// from a prior bootstrap.
func resolveProfileForTenant(profileFlag, nameFlag string) (string, error) {
	pf := profileFlag
	if pf == "" {
		pf = nameFlag
	}
	resolved, err := ResolveProfile(pf)
	if err != nil {
		return "", err
	}
	if resolved == ActiveNone {
		return "", nil
	}
	return resolved, nil
}

// runTenants lists the tenants the signed identity can see. The
// server filters by membership (super_admin sees all). Mirrors
// `txco auth profiles` in shape: marks the active tenant with *.
func runTenants(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenants", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth tenants [flags]

List the tenants you have membership in (super_admins see all).
The active tenant (from --tenant, TXCO_TENANT, or meta's
default_tenant) is marked with `+"`*`"+`.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenants: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenants: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth tenants: no signing key configured; listing requires authentication")
		return 1
	}

	rows, err := client.New(target).ListTenants(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "auth tenants: %v\n", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "(no tenants — ask a super-admin for an invitation)")
		return 0
	}
	active := ResolveTenant("", resolvedProfile)

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tSLUG\tNAME\tTENANT_ID")
	for _, t := range rows {
		mark := " "
		if t.Slug == active {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, t.Slug, dashIfEmpty(t.Name), t.TenantID)
	}
	_ = tw.Flush()
	return 0
}

// runTenantCmd dispatches `txco auth tenant <sub>`. Mirrors the
// existing `auth profile` shape: tidy namespace, related verbs
// grouped together. v1 ships create + members; rename/destroy are
// follow-ups when the use case appears.
func runTenantCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTenantUsage(stdout)
		return 0
	}
	switch args[0] {
	case "create":
		return runTenantCreate(args[1:], stdout, stderr)
	case "members":
		return runTenantMembers(args[1:], stdout, stderr)
	case "grant", "update-caps":
		// update-caps is an alias for grant — CreateMembership is an
		// upsert on the server, so promote-via-grant works either way.
		return runTenantGrant(args[1:], stdout, stderr)
	case "revoke":
		return runTenantRevoke(args[1:], stdout, stderr)
	case "hostnames":
		return runTenantHostnames(args[1:], stdout, stderr)
	case "secrets":
		return runTenantSecrets(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printTenantUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth tenant: unknown subcommand %q\n\n", args[0])
		printTenantUsage(stderr)
		return 2
	}
}

func printTenantUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco auth tenant [flags]
  txco auth tenant <command> [flags]

Manage tenants (the chassis's authorization scopes).

Available commands:
  create <slug> [--name NAME]            Mint a new tenant (super_admin only)
  members [--tenant SLUG]                List members of one tenant
  grant <actor> --caps "..." [--tenant]  Upsert a membership in one round-trip
  update-caps <actor> --caps "..."       Alias for grant (same wire call)
  revoke <actor> [--tenant SLUG] [-y]    Soft-delete a membership
  hostnames <sub>                        Manage hostname → tenant routing
  secrets <sub>                          Manage per-tenant secrets (Stripe keys, OAuth tokens, …)

Related:
  txco auth tenants             List tenants you can see
  txco auth memberships         Your tenant memberships across the chassis

Use `+"`txco auth tenant <command> --help`"+` for per-command flags.
`)
}

// runTenantCreate mints a new tenant. Super-admin only on the server
// side; the CLI just rounds the verb to a POST. The freshly-minted
// row is echoed so scripts can capture tenant_id.
func runTenantCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	displayName := fs.String("display-name", "", "human-readable name for the tenant")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth tenant create <slug> [--display-name NAME]

Create a new tenant with the given slug. Slug is lowercased; it's the
durable handle every subsequent --tenant flag refers to. Requires
super_admin (or basic-auth / open operator).

Flags:
`)
		fs.PrintDefaults()
	}
	// Pre-extract the slug if it's the first positional, so users can
	// write `tenant create <slug> --display-name X` OR put flags
	// first. Go's stdlib flag parser stops at the first non-flag, so
	// without this nudge the slug would shadow every following flag.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append([]string{}, args...)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth tenant create: slug is required")
		fs.Usage()
		return 2
	}
	slug := fs.Arg(0)
	// Re-parse the remaining args so flags placed AFTER the slug also
	// get picked up. fs.Parse on the trailing slice consumes only
	// flags; the slug we already captured is out of the way.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant create: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant create: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth tenant create: no signing key configured; creating a tenant requires super_admin authentication")
		return 1
	}
	t, err := client.New(target).CreateTenant(context.Background(), client.CreateTenantRequest{
		Slug: slug,
		Name: *displayName,
	})
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "tenant:    %s\n", t.Slug)
	if t.Name != "" {
		fmt.Fprintf(stdout, "name:      %s\n", t.Name)
	}
	fmt.Fprintf(stdout, "tenant_id: %s\n", t.TenantID)
	fmt.Fprintln(stdout, "")
	fmt.Fprintf(stdout, "next: txco auth invite --tenant %s --label <name>\n", t.Slug)
	return 0
}

// runTenantMembers lists members of the target tenant. Tenant resolution
// follows the standard precedence (--tenant > TXCO_TENANT > meta's
// default_tenant > literal "default") so script-style invocations
// default to the active tenant without flags.
func runTenantMembers(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant members", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug whose members to list")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth tenant members [--tenant SLUG]

List the active members of one tenant. Defaults to the active
tenant when --tenant is omitted. Requires actor:read in the
target tenant.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant members: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant members: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth tenant members: no signing key configured; listing requires authentication")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	members, err := client.New(target).ListTenantMembers(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant members: %v\n", err)
		return 1
	}
	if len(members) == 0 {
		fmt.Fprintf(stdout, "(no members in tenant %q)\n", target.Tenant)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTOR_ID\tLABEL\tCAPABILITIES\tSINCE")
	for _, m := range members {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			m.ActorID, dashIfEmpty(m.Label),
			strings.Join(m.Capabilities, ","), m.CreatedAt)
	}
	_ = tw.Flush()
	return 0
}

// runMemberships shows the caller's memberships across all tenants.
// Reads from `txco auth whoami` so it's just one round-trip (the
// server already returns memberships there in phase 5).
func runMemberships(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth memberships", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth memberships [flags]

Show your tenant memberships and the capabilities each one grants.
Equivalent to the memberships block of `+"`txco auth whoami`"+`, listed on
its own so scripts can grep it without parsing the identity header.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth memberships: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth memberships: %v\n", err)
		return 1
	}
	resp, err := client.New(target).Whoami(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "auth memberships: %v\n", err)
		return 1
	}
	if resp.SuperAdmin {
		fmt.Fprintln(stdout, "super_admin: true (chassis-wide; passes every capability check)")
		fmt.Fprintln(stdout)
	}
	if len(resp.Memberships) == 0 {
		if resp.Source != "signed" {
			fmt.Fprintln(stdout, "(no signed identity; memberships only available for signed callers)")
		} else {
			fmt.Fprintln(stdout, "(no memberships)")
		}
		return 0
	}
	active := ResolveTenant("", resolvedProfile)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTIVE\tTENANT\tCAPABILITIES")
	for _, m := range resp.Memberships {
		mark := " "
		if m.TenantSlug == active {
			mark = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, m.TenantSlug, strings.Join(m.Capabilities, ","))
	}
	_ = tw.Flush()
	return 0
}

// runTenantGrant upserts a membership. v1 capability strings are
// 3-segment (`domain:instance:action`); the CLI parses --caps locally
// via policy.ParseCapabilities so typos error before the http round-
// trip. Server-side CreateMembership is an upsert keyed on
// (actor_id, tenant_id), so re-running grant with different caps
// promotes/demotes in place — no separate update-caps verb needed,
// though we register one as an alias for muscle memory.
func runTenantGrant(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant grant", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug to grant in")
	caps := fs.String("caps", "", "comma-separated capabilities (e.g. \"opstack:*:read,actor:*:read\")")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth tenant grant <actor_id> --caps "..." [--tenant SLUG]

Upsert a membership: <actor_id> in the target tenant with the given
capability set. Replaces any prior capabilities for the same
(actor, tenant) pair. Requires actor:*:invite in the tenant.

Capability strings follow Apache Shiro's domain:instance:action shape;
v1 uses `+"`*`"+` for the instance segment everywhere. See `+"`docs/auth.md`"+` for
the full whitelist.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth tenant grant: actor_id is required")
		fs.Usage()
		return 2
	}
	actorID := fs.Arg(0)
	// Re-parse remaining args so flags placed AFTER the positional
	// also get picked up (Go's stdlib flag parser stops at the first
	// non-flag; the positional was already captured above).
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if strings.TrimSpace(*caps) == "" {
		fmt.Fprintln(stderr, "auth tenant grant: --caps is required")
		fs.Usage()
		return 2
	}
	parsed, err := policy.ParseCapabilities(*caps)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant grant: %v\n", err)
		return 2
	}
	if len(parsed) == 0 {
		fmt.Fprintln(stderr, "auth tenant grant: --caps parsed to an empty set")
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant grant: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant grant: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth tenant grant: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	m, err := client.New(target).GrantMember(context.Background(), client.GrantMemberRequest{
		ActorID:      actorID,
		Capabilities: parsed,
	})
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant grant: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "granted %s in tenant %s:\n", m.ActorID, target.Tenant)
	if m.Label != "" {
		fmt.Fprintf(stdout, "  label: %s\n", m.Label)
	}
	fmt.Fprintf(stdout, "  caps:  %s\n", strings.Join(m.Capabilities, ","))
	return 0
}

// runTenantHostnames dispatches `txco auth tenant hostnames <sub>`.
// Three verbs: list / add / remove. The chassis owns the canonical
// form so the CLI is mostly a thin pass-through.
func runTenantHostnames(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHostnamesUsage(stdout)
		return 0
	}
	switch args[0] {
	case "list":
		return runHostnamesList(args[1:], stdout, stderr)
	case "add":
		return runHostnamesAdd(args[1:], stdout, stderr)
	case "remove", "rm":
		return runHostnamesRemove(args[1:], stdout, stderr)
	case "attach":
		return runHostnamesAttach(args[1:], stdout, stderr)
	case "challenge":
		return runHostnamesChallenge(args[1:], stdout, stderr)
	case "verify":
		return runHostnamesVerify(args[1:], stdout, stderr)
	case "status", "show":
		return runHostnamesStatus(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printHostnamesUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth tenant hostnames: unknown subcommand %q\n\n", args[0])
		printHostnamesUsage(stderr)
		return 2
	}
}

func printHostnamesUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco auth tenant hostnames <command> [flags]

Manage hostname → tenant routing. Each binding maps an HTTP Host
header to a (tenant, stack) the data plane dispatches to.

Available commands:
  list [--tenant SLUG] [--history]       List bindings for a tenant
  add <hostname> [--stack <stack>] [--tenant SLUG]
                                         Claim a hostname; --stack
                                         attaches in one step (shortcut)
  attach <hostname> --stack <stack> [--tenant SLUG]
                                         Bind a claimed hostname to a stack
  remove <hostname> [--tenant SLUG]      Release a hostname (soft-delete)
  challenge <hostname> [--method {dns-txt|http-01}] [--rotate] [--tenant SLUG]
                                         Issue a verification challenge (default dns-txt).
                                         Idempotent: reuses an active non-expired
                                         challenge unless --rotate is given.
  verify <hostname> [--tenant SLUG]      Attempt the active challenge
  status <hostname> [--tenant SLUG]      Read-only: show verification state +
                                         the current active token (no mutation)

`)
}

func runHostnamesList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug whose hostnames to list")
	history := fs.Bool("history", false, "include revoked rows")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames list: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames list: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames list: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	rows, err := client.New(target).ListHostnames(context.Background(), *history)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames list: %v", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "(no hostnames in tenant %q)\n", target.Tenant)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if *history {
		fmt.Fprintln(tw, "HOSTNAME\tSTACK\tVERIFIED\tCREATED_AT\tCREATED_BY\tREVOKED_AT")
		for _, h := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				h.Hostname, h.Stack, dashIfEmpty(h.VerifiedAt), h.CreatedAt,
				dashIfEmpty(h.CreatedBy), dashIfEmpty(h.RevokedAt))
		}
	} else {
		fmt.Fprintln(tw, "HOSTNAME\tSTACK\tVERIFIED\tCREATED_AT\tCREATED_BY")
		for _, h := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				h.Hostname, h.Stack, dashIfEmpty(h.VerifiedAt), h.CreatedAt,
				dashIfEmpty(h.CreatedBy))
		}
	}
	_ = tw.Flush()
	return 0
}

func runHostnamesAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug to claim the hostname in")
	stack := fs.String("stack", "", "stack within the tenant the hostname routes to")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames add: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	// Re-parse trailing flags placed after the positional.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	// --stack is now OPTIONAL. Without it, the hostname is claimed
	// unattached: the tenant can verify ownership now and attach it
	// to a stack later via `attach`. With it, this verb is the
	// create+attach shortcut for tenants who already know the
	// routing target.

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames add: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames add: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames add: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	cli := client.New(target)
	h, err := cli.AddHostname(context.Background(), client.AddHostnameRequest{
		Hostname: hostname,
		Stack:    strings.TrimSpace(*stack),
	})
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames add: %v", err)
		return 1
	}
	if h.Stack == "" {
		fmt.Fprintf(stdout, "claimed %s (unattached)\n", h.Hostname)
	} else {
		fmt.Fprintf(stdout, "claimed %s → %s/%s\n", h.Hostname, target.Tenant, h.Stack)
	}

	// Dev-local auto-verify path: when the chassis stamped verified_at
	// on creation (localhost, *.localhost, *.local, *.local.thanks.
	// computer; gated by --dev-auto-verify-local-hostnames, default on),
	// the operator self-evidently owns the hostname — no DNS-TXT proof
	// needed. Skip the auto-challenge and the DNS-record reminder
	// entirely; print a one-liner so the operator knows it routed and
	// is ready to receive traffic.
	if h.VerifiedAt != "" {
		fmt.Fprintf(stdout, "auto-verified (dev-local): routes immediately, no DNS-TXT step required.\n")
		if h.Stack == "" {
			fmt.Fprintf(stdout, "\nthen attach: txco auth tenant hostnames attach %s --stack <name>\n", h.Hostname)
		}
		return 0
	}

	// Auto-issue a dns-txt challenge so the operator sees the
	// next-step instructions immediately. Verification is required
	// either way (strict-mode chassis won't route without it); the
	// add verb is the right place to surface it without making the
	// user run a second command just to get the TXT record value.
	//
	// force=false: the challenge endpoint is idempotent — if a prior
	// active challenge already exists for this (hostname, dns-txt) the
	// server returns IT instead of rotating. So re-running `add` after
	// a typo doesn't invalidate a TXT the operator already pasted into
	// DNS. (internal docs/todo-custom-domains.md §6a.)
	//
	// If challenge issuance fails (e.g. server rate-limit, transient
	// error), the hostname row is still claimed — print a warning
	// and tell the operator how to retry. Don't fail the whole add.
	ch, chErr := cli.CreateHostnameChallenge(context.Background(), h.Hostname, "dns-txt", false)
	if chErr != nil {
		fmt.Fprintf(stderr,
			"warning: auto-challenge failed: %v\n  retry with: txco auth tenant hostnames challenge %s\n",
			chErr, h.Hostname)
		return 0
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintf(stdout, "next step — verify ownership (method=%s):\n", ch.Method)
	fmt.Fprintf(stdout, "  token:      %s\n", ch.Token)
	fmt.Fprintf(stdout, "  expires_at: %s\n", ch.ExpiresAt)
	if ch.Reused {
		fmt.Fprintln(stdout, "  (reused active challenge — token unchanged from prior issuance)")
	}
	fmt.Fprintln(stdout, "")
	if ch.Instructions != "" {
		fmt.Fprintln(stdout, ch.Instructions)
	}

	// Routing-record reminder. The TXT proves ownership but does NOT
	// route traffic — that's what bit us when test2.loremlabs.com
	// `verify` succeeded but `curl` failed with "Could not resolve
	// host" (no CNAME/A pointed at the chassis). Kept generic so
	// open-core doesn't hardcode an operator-specific CNAME target
	// (different deployments, different target hostnames); the
	// operator's docs spell out the exact value.
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "also route traffic for this hostname to the chassis:")
	fmt.Fprintf(stdout, "  CNAME %s. -> <your operator's custom-domain target>\n", h.Hostname)
	fmt.Fprintln(stdout, "  (or an A record to the chassis IP for an apex domain)")
	fmt.Fprintln(stdout, "The TXT above ONLY proves ownership; it does NOT route traffic.")

	if h.Stack == "" {
		fmt.Fprintf(stdout, "\nthen attach: txco auth tenant hostnames attach %s --stack <name>\n", h.Hostname)
	}
	return 0
}

// runHostnamesAttach binds an existing hostname row to a stack. The
// row must already be claimed (via `add`) — this is the second half
// of the Vercel-style decoupled flow. Re-running with a different
// stack swaps the routing target; no `detach` verb in v1.
func runHostnamesAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug owning the hostname")
	stack := fs.String("stack", "", "stack the hostname should route to (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames attach: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if strings.TrimSpace(*stack) == "" {
		PrintCLIError(stderr, "auth tenant hostnames attach: --stack is required")
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames attach: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames attach: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames attach: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	h, err := client.New(target).AttachHostname(context.Background(), hostname, strings.TrimSpace(*stack))
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames attach: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "attached %s → %s/%s\n", h.Hostname, target.Tenant, h.Stack)
	if h.VerifiedAt == "" {
		fmt.Fprintln(stdout, "  (hostname is not verified yet — routing will not honor it until you run `txco auth tenant hostnames verify` and (in production) --require-hostname-verification is set)")
	}
	return 0
}

func runHostnamesChallenge(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames challenge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug owning the hostname")
	// dns-txt is the default because it works before DNS is pointed
	// at the chassis — useful for ahead-of-cutover verification. The
	// operator opts into http-01 explicitly when DNS already points
	// here.
	method := fs.String("method", "dns-txt", "verification method: dns-txt or http-01")
	// --rotate (alias --force): opt back into the pre-fast-follow
	// behaviour where every challenge revokes the prior and mints a
	// new token. Without it, the server reuses an active non-expired
	// challenge so the operator can run `challenge` ten times and the
	// TXT they pasted into DNS stays valid (internal docs/todo-custom-domains.md
	// §6a).
	rotate := fs.Bool("rotate", false, "revoke the active token and mint a new one (alias --force)")
	force := fs.Bool("force", false, "alias for --rotate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames challenge: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *method != "dns-txt" && *method != "http-01" {
		PrintCLIError(stderr, "auth tenant hostnames challenge: --method must be 'dns-txt' or 'http-01'")
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames challenge: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames challenge: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames challenge: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	wantRotate := *rotate || *force
	ch, err := client.New(target).CreateHostnameChallenge(context.Background(), hostname, *method, wantRotate)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames challenge: %v", err)
		return 1
	}
	switch {
	case ch.Reused:
		fmt.Fprintf(stdout, "reused active challenge for %s (method=%s) — token unchanged\n", hostname, ch.Method)
	case ch.Rotated:
		// Loud: this is the operator-hostile branch — anything they
		// pasted into DNS from the prior token is now invalid.
		fmt.Fprintf(stdout, "⚠ rotated challenge for %s (method=%s) — previous token revoked\n", hostname, ch.Method)
		fmt.Fprintf(stdout, "  any DNS TXT with the old token is now invalid; update it to the new token below.\n")
	default:
		fmt.Fprintf(stdout, "challenge issued for %s (method=%s)\n", hostname, ch.Method)
	}
	fmt.Fprintf(stdout, "  id:         %s\n", ch.ID)
	fmt.Fprintf(stdout, "  token:      %s\n", ch.Token)
	fmt.Fprintf(stdout, "  expires_at: %s\n\n", ch.ExpiresAt)
	if ch.Instructions != "" {
		fmt.Fprintln(stdout, ch.Instructions)
		// Footgun hint: explicit so operators understand the
		// idempotent contract without reading the docs.
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "(The TXT must contain THIS token. Re-running `challenge` reuses it; --rotate changes it.)")
	}
	return 0
}

func runHostnamesVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug owning the hostname")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames verify: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames verify: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames verify: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames verify: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	resp, err := client.New(target).VerifyHostname(context.Background(), hostname)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames verify: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "verified %s (method=%s) at %s\n", hostname, resp.Method, resp.VerifiedAt)
	return 0
}

func runHostnamesRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug owning the hostname")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames remove: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames remove: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames remove: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames remove: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	if err := client.New(target).RemoveHostname(context.Background(), hostname); err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames remove: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "released %s from tenant %s\n", hostname, target.Tenant)
	return 0
}

// runHostnamesStatus is the read-only counterpart to `challenge` — it
// reads the current binding + active challenge(s) without mutating
// anything. Closes the rotation footgun documented at
// internal docs/todo-custom-domains.md §6a (pre-status, the only way to "see"
// the current verify token was `challenge`, which rotated it).
func runHostnamesStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant hostnames status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug owning the hostname")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant hostnames status: hostname is required")
		return 2
	}
	hostname := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames status: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames status: %v", err)
		return 1
	}
	if target.Auth == nil {
		PrintCLIError(stderr, "auth tenant hostnames status: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	st, err := client.New(target).HostnameStatusOf(context.Background(), hostname)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant hostnames status: %v", err)
		return 1
	}
	// Header: routing-relevant facts (operator-friendly order). Verified
	// + stack is what determines whether the resolver routes; surface
	// that prominently with a one-glance state line.
	state := "claimed (not verified)"
	switch {
	case st.RevokedAt != "":
		state = "revoked"
	case st.VerifiedAt != "" && st.Stack != "":
		state = "verified + attached → routing"
	case st.VerifiedAt != "":
		state = "verified (no stack attached — won't route)"
	case st.Stack != "":
		state = "claimed → " + st.Stack + " (not verified — won't route in strict mode)"
	}
	fmt.Fprintf(stdout, "%s\n", st.Hostname.Hostname)
	fmt.Fprintf(stdout, "  state:       %s\n", state)
	if st.Stack != "" {
		fmt.Fprintf(stdout, "  stack:       %s\n", st.Stack)
	}
	if st.VerifiedAt != "" {
		fmt.Fprintf(stdout, "  verified_at: %s\n", st.VerifiedAt)
	}
	if st.RevokedAt != "" {
		fmt.Fprintf(stdout, "  revoked_at:  %s\n", st.RevokedAt)
	}
	fmt.Fprintf(stdout, "  created_at:  %s\n", st.CreatedAt)
	if st.CreatedBy != "" {
		fmt.Fprintf(stdout, "  created_by:  %s\n", st.CreatedBy)
	}

	if len(st.ActiveChallenges) == 0 {
		fmt.Fprintln(stdout, "\nno active challenge.")
		if st.VerifiedAt == "" {
			fmt.Fprintf(stdout, "  next step: txco auth tenant hostnames challenge %s\n", hostname)
		}
		return 0
	}
	fmt.Fprintln(stdout, "\nactive challenge(s):")
	for _, c := range st.ActiveChallenges {
		tag := ""
		if c.Expired {
			tag = " (EXPIRED — re-issue with `challenge --rotate`)"
		}
		fmt.Fprintf(stdout, "  method=%s%s\n", c.Method, tag)
		fmt.Fprintf(stdout, "    token:       %s\n", c.Token)
		fmt.Fprintf(stdout, "    expires_at:  %s\n", c.ExpiresAt)
		if c.AttemptedAt != "" {
			fmt.Fprintf(stdout, "    attempted:   %s\n", c.AttemptedAt)
		}
		if c.LastError != "" {
			fmt.Fprintf(stdout, "    last_error:  %s\n", c.LastError)
		}
		if c.Instructions != "" {
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, indent(c.Instructions, "    "))
		}
	}
	return 0
}

// indent prefixes every line of s with prefix. Tiny local helper so
// the status output can inline-format the multi-line instructions
// block under each challenge without adding a strings dep churn.
func indent(s, prefix string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if ln == "" {
			continue
		}
		lines[i] = prefix + ln
	}
	return strings.Join(lines, "\n")
}

// runTenantRevoke soft-deletes a membership. The actor's key and
// chassis-wide identity are untouched — only their seat in this
// tenant. Prompts for confirmation by default; -y skips the prompt
// (parity with `txco auth profile remove`).
func runTenantRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint (defaults to meta's chassis_url)")
	profile := fs.String("profile", "", fmt.Sprintf("profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", HomePathPretty()))
	name := fs.String("name", "", "alias for --profile (kept for back-compat)")
	tenant := fs.String("tenant", "", "tenant slug to revoke from")
	force := fs.Bool("y", false, "skip the confirmation prompt")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth tenant revoke <actor_id> [--tenant SLUG] [-y]

Soft-revoke <actor_id>'s membership in the target tenant. The actor's
key + identity stay intact; only their access to this tenant goes
away. Requires actor:*:invite in the tenant.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "auth tenant revoke: actor_id is required")
		fs.Usage()
		return 2
	}
	actorID := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}

	resolvedProfile, err := resolveProfileForTenant(*profile, *name)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant revoke: %v\n", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		fmt.Fprintf(stderr, "auth tenant revoke: %v\n", err)
		return 1
	}
	if target.Auth == nil {
		fmt.Fprintln(stderr, "auth tenant revoke: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	if !*force {
		fmt.Fprintf(stderr, "revoke %s's membership in tenant %s? [y/N]: ", actorID, target.Tenant)
		if !promptYesNo(os.Stdin, stderr, "", false) {
			fmt.Fprintln(stdout, "aborted")
			return 0
		}
	}
	if err := client.New(target).RevokeMember(context.Background(), actorID); err != nil {
		fmt.Fprintf(stderr, "auth tenant revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s in tenant %s\n", actorID, target.Tenant)
	return 0
}

