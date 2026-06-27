package auth

// Tenant-scoped CLI for the per-tenant secret store. Dispatched
// from runTenantCmd ("auth tenant"). Operator-supplied values (set,
// rotate) prompt via TTY hidden input — never on the command line.
// Generated values (generate, rotate --generate) are printed exactly
// once.
//
// See internal docs/todo-secret-store.md §6 (CLI surface).
//
// The operator-level master-key command (`txco auth secrets init`)
// lives in secrets_cmd.go — different scope, different file.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runTenantSecrets dispatches `txco auth tenant secrets <sub>`.
func runTenantSecrets(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTenantSecretsUsage(stdout)
		return 0
	}
	switch args[0] {
	case "set":
		return runSecretsSet(args[1:], stdout, stderr)
	case "generate", "gen":
		return runSecretsGenerate(args[1:], stdout, stderr)
	case "rotate":
		return runSecretsRotate(args[1:], stdout, stderr)
	case "list", "ls":
		return runSecretsList(args[1:], stdout, stderr)
	case "show":
		return runSecretsShow(args[1:], stdout, stderr)
	case "describe":
		return runSecretsDescribe(args[1:], stdout, stderr)
	case "revoke", "rm":
		return runSecretsRevoke(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printTenantSecretsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth tenant secrets: unknown subcommand %q\n\n", args[0])
		printTenantSecretsUsage(stderr)
		return 2
	}
}

func printTenantSecretsUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco auth tenant secrets <command> [flags]

Manage per-tenant secrets (Stripe API keys, OAuth tokens, signing
keys, etc.). Secrets are referenced by name from txcl ops via the
WITH clause; the value never leaves the chassis process.

Available commands:
  set NAME [--tenant SLUG] [--stack S] [--description STR]
                                  Store an operator-supplied value.
                                  Prompts for the value via TTY (hidden).
                                  Creates if absent, rotates if present.
  generate NAME [--tenant SLUG] [--stack S] [--byte-len N]
                                  Mint a fresh random value (default 32B);
                                  print the value ONCE and store it.
  rotate NAME [--tenant SLUG] [--stack S] [--generate] [--byte-len N]
                                  Rotate to a new value. Operator-supplied
                                  by default (TTY prompt); --generate mints
                                  random and prints once.
  list [--tenant SLUG]            List active secrets in a tenant (metadata
                                  only — values are NEVER shown).
  show NAME [--tenant SLUG] [--stack S]
                                  Show metadata for one secret.
  describe NAME --set "TEXT" [--tenant SLUG] [--stack S]
                                  Update the description. Names are
                                  immutable — rename = generate-new +
                                  revoke-old.
  revoke NAME [--tenant SLUG] [--stack S]
                                  Soft-delete (the name is freed for
                                  re-creation; old encrypted versions
                                  stay for audit).

To inspect a value: there is no reveal command. Rotate the secret
(or generate a fresh one); both print the value exactly once.

`)
}

// readValueFromTTY prompts the operator for the cleartext via the
// same hidden-input path resolveSecret uses for the dev-enroll secret.
// Empty input is rejected.
func readValueFromTTY(label string, stderr io.Writer) (string, error) {
	val, err := resolveSecret("", stderr) // empty flagValue → TTY prompt
	_ = label                             // resolveSecret has its own prompt text
	if err != nil {
		return "", err
	}
	val = strings.TrimRight(val, "\n")
	if val == "" {
		return "", fmt.Errorf("empty value")
	}
	return val, nil
}

func runSecretsSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	desc := fs.String("description", "", "operator-visible description")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets set: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	// Re-parse trailing flags placed after the positional NAME. Go's
	// stdlib flag pkg stops at the first non-flag arg, so `set FOO
	// --tenant default --url ...` would otherwise silently drop the
	// flags. Mirrors the pattern in tenant_cmd.go (e.g. runHostnamesAdd).
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets set: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets set: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets set: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	if err := ConfirmTargetStd(resolvedProfile, target.Addr, *yes, false, stderr); err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets set: %v", err)
		return 1
	}

	value, err := readValueFromTTY(name, stderr)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets set: %v", err)
		return 1
	}

	cli := client.New(target)
	// Try create first; on 409 (exists), rotate.
	created, cerr := cli.CreateSecret(context.Background(), client.CreateSecretRequest{
		Name:        name,
		Value:       value,
		Description: *desc,
		Stack:       *stack,
	})
	if cerr == nil {
		fmt.Fprintf(stdout, "created %s (v%d) in tenant %q\n", created.Name, created.VersionNo, target.Tenant)
		return 0
	}
	// Detect 409 / secret_exists; fall through to rotate.
	if he, ok := cerr.(*client.HTTPError); ok && he.Code == "secret_exists" {
		rotated, rerr := cli.RotateSecret(context.Background(), name, *stack, value)
		if rerr != nil {
			PrintCLIErrorf(stderr, "auth tenant secrets set: rotate: %v", rerr)
			return 1
		}
		fmt.Fprintf(stdout, "rotated %s → v%d in tenant %q\n", rotated.Name, rotated.VersionNo, target.Tenant)
		return 0
	}
	PrintCLIErrorf(stderr, "auth tenant secrets set: %v", cerr)
	return 1
}

func runSecretsGenerate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets generate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	desc := fs.String("description", "", "operator-visible description")
	byteLen := fs.Int("byte-len", 32, "number of random bytes to mint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets generate: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets generate: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets generate: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets generate: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	cli := client.New(target)
	res, err := cli.GenerateSecret(context.Background(), client.GenerateSecretRequest{
		Name:        name,
		Description: *desc,
		Stack:       *stack,
		ByteLen:     *byteLen,
	})
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets generate: %v", err)
		return 1
	}
	// Reveal-once: print the value to stdout. Operator captures it
	// here or loses it. The trailing newline is intentional — keeps
	// "copy from terminal" clean across shell selections.
	fmt.Fprintln(stdout, res.Value)
	fmt.Fprintf(stderr, "generated %s (v%d) in tenant %q. The value above is shown ONCE — rotate to see again.\n",
		res.Secret.Name, res.Secret.VersionNo, target.Tenant)
	return 0
}

func runSecretsRotate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	generate := fs.Bool("generate", false, "mint a random value instead of prompting (prints once)")
	byteLen := fs.Int("byte-len", 32, "number of random bytes (only with --generate)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets rotate: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets rotate: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets rotate: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets rotate: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	if err := ConfirmTargetStd(resolvedProfile, target.Addr, *yes, false, stderr); err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets rotate: %v", err)
		return 1
	}
	cli := client.New(target)

	if *generate {
		res, gerr := cli.RotateSecretGenerated(context.Background(), name, *stack, *byteLen)
		if gerr != nil {
			PrintCLIErrorf(stderr, "auth tenant secrets rotate --generate: %v", gerr)
			return 1
		}
		fmt.Fprintln(stdout, res.Value)
		fmt.Fprintf(stderr, "rotated %s → v%d in tenant %q. The value above is shown ONCE.\n",
			res.Secret.Name, res.Secret.VersionNo, target.Tenant)
		return 0
	}

	value, err := readValueFromTTY(name, stderr)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets rotate: %v", err)
		return 1
	}
	res, err := cli.RotateSecret(context.Background(), name, *stack, value)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets rotate: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "rotated %s → v%d in tenant %q\n", res.Name, res.VersionNo, target.Tenant)
	return 0
}

func runSecretsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets list: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets list: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets list: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	rows, err := client.New(target).ListSecrets(context.Background())
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets list: %v", err)
		return 1
	}
	if len(rows) == 0 {
		fmt.Fprintf(stdout, "(no secrets in tenant %q)\n", target.Tenant)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTACK\tVERSION\tCREATED_AT\tLAST_ROTATED\tDESCRIPTION")
	for _, s := range rows {
		fmt.Fprintf(tw, "%s\t%s\tv%d\t%s\t%s\t%s\n",
			s.Name, dashIfEmpty(s.Stack), s.VersionNo,
			s.CreatedAt, dashIfEmpty(s.LastRotatedAt), dashIfEmpty(s.Description))
	}
	_ = tw.Flush()
	return 0
}

func runSecretsShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets show: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets show: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets show: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets show: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	s, err := client.New(target).GetSecret(context.Background(), name, *stack)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets show: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "name:           %s\n", s.Name)
	fmt.Fprintf(stdout, "stack:          %s\n", dashIfEmpty(s.Stack))
	fmt.Fprintf(stdout, "version:        v%d\n", s.VersionNo)
	fmt.Fprintf(stdout, "key_version:    %d\n", s.KeyVersion)
	fmt.Fprintf(stdout, "description:    %s\n", dashIfEmpty(s.Description))
	fmt.Fprintf(stdout, "created_at:     %s\n", s.CreatedAt)
	fmt.Fprintf(stdout, "created_by:     %s\n", dashIfEmpty(s.CreatedBy))
	fmt.Fprintf(stdout, "last_rotated:   %s\n", dashIfEmpty(s.LastRotatedAt))
	fmt.Fprintf(stdout, "secret_id:      %s\n", s.SecretID)
	return 0
}

func runSecretsDescribe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets describe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	newDesc := fs.String("set", "", "new description text (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets describe: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	// Re-parse trailing flags BEFORE the --set Visit below — otherwise
	// `describe FOO --set "new desc"` silently drops --set.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}
	// `--set` not provided → reject. Updating description-to-empty is
	// allowed if the operator explicitly passes --set="".
	wasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "set" {
			wasSet = true
		}
	})
	if !wasSet {
		PrintCLIError(stderr, "auth tenant secrets describe: --set is required")
		return 2
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets describe: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets describe: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets describe: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)

	s, err := client.New(target).UpdateSecretDescription(context.Background(), name, *stack, *newDesc)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets describe: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "updated %s description: %q\n", s.Name, s.Description)
	return 0
}

func runSecretsRevoke(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth tenant secrets revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "", "chassis admin endpoint")
	profile := fs.String("profile", "", "profile name")
	targetSel := fs.String("target", "", "chassis to act on: a profile name or a raw admin URL")
	tenant := fs.String("tenant", "", "tenant slug")
	stack := fs.String("stack", "", "stack scope (empty = tenant-wide)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		PrintCLIError(stderr, "auth tenant secrets revoke: NAME is required")
		return 2
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if *targetSel == "" { // a trailing positional selects the target, e.g. `secrets set NAME staging`
		*targetSel = trailingPositional(fs)
	}

	applyTargetSelector(*targetSel, url, profile)
	resolvedProfile, err := resolveProfileForTenant(*profile, "")
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets revoke: %v", err)
		return 1
	}
	target, err := buildSignedTarget(resolvedProfile, *url)
	if err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets revoke: %v", err)
		return 1
	}
	if target.Auth == nil && !LocalChassis(target.Addr) {
		PrintCLIError(stderr, "auth tenant secrets revoke: no signing key configured")
		return 1
	}
	target.Tenant = ResolveTenant(*tenant, resolvedProfile)
	if err := ConfirmTargetStd(resolvedProfile, target.Addr, *yes, false, stderr); err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets revoke: %v", err)
		return 1
	}

	if err := client.New(target).RevokeSecret(context.Background(), name, *stack); err != nil {
		PrintCLIErrorf(stderr, "auth tenant secrets revoke: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s from tenant %q (name freed for re-creation)\n", name, target.Tenant)
	return 0
}

// keep os imported even though we currently don't use it directly
// here — needed when readValueFromTTY is exercised in non-test paths
// that fall back to os.Stdin via resolveSecret.
var _ = os.Stdin
