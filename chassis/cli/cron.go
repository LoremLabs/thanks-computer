package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runCron routes `txco cron <subcommand> ...`.
func runCron(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printCronUsage(stdout)
		return 0
	}
	switch args[0] {
	case "config":
		return runCronConfig(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printCronUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "cron: unknown subcommand %q\n\n", args[0])
		printCronUsage(stderr)
		return 2
	}
}

func printCronUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco cron <subcommand> ...

Subcommands:
  config show                 Show this tenant's cron timezone (UTC if unset)
  config set timezone <zone>  Set the cron timezone (IANA name, e.g. Asia/Tokyo)
  config set timezone ""      Clear it (back to UTC)

The cron head stamps @cron.hour/minute/... in this timezone; @cron.bucket
stays UTC. Run 'txco cron config set --help' for flags.
`)
}

// cronFlags bundles the common target/auth flags (mirrors dnsFlags); the
// timezone setting is tenant-scoped, so --tenant selects which tenant.
type cronFlags struct {
	target, addr, user, pass, profile, tenant *string
}

func registerCronFlags(fs *flag.FlagSet) cronFlags {
	return cronFlags{
		target:  fs.String("target", "", "target name from txco.yaml"),
		addr:    fs.String("addr", "", "chassis admin endpoint"),
		user:    fs.String("user", "", "basic auth user"),
		pass:    fs.String("pass", "", "basic auth password"),
		profile: fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty())),
		tenant:  fs.String("tenant", "", "tenant slug"),
	}
}

func (f cronFlags) client() *client.Client {
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	t.Tenant = resolveTenant(*f.tenant, *f.profile)
	return client.New(t)
}

// cliCronConfigDTO mirrors the admin cronConfigDTO.
type cliCronConfigDTO struct {
	Timezone   string `json:"timezone"`
	Configured bool   `json:"configured"`
}

func runCronConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "cron config: expected show|set")
		return 2
	}
	switch args[0] {
	case "show", "get":
		return runCronConfigShow(args[1:], stdout, stderr)
	case "set":
		return runCronConfigSet(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "cron config: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runCronConfigShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cron config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerCronFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var dto cliCronConfigDTO
	if err := f.client().DoScoped(context.Background(), "GET", "/cron/config", nil, &dto); err != nil {
		fmt.Fprintf(stderr, "cron config show: %v\n", err)
		return 1
	}
	tz := dto.Timezone
	if tz == "" {
		tz = "UTC (default)"
	}
	fmt.Fprintf(stdout, "cron timezone: %s\n", tz)
	return 0
}

func runCronConfigSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cron config set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tzFlag := fs.String("timezone", "", `IANA timezone (e.g. Asia/Tokyo); empty clears → UTC`)
	f := registerCronFlags(fs)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco cron config set timezone <IANA zone>\n   or: txco cron config set --timezone <IANA zone>\n\nFlags:\n")
		fs.PrintDefaults()
	}

	// Accept the positional form `set timezone <zone>` (what the docs show)
	// as well as `--timezone <zone>`; auth flags may follow either.
	rest := args
	tz := ""
	posGiven := false
	if len(args) >= 1 && args[0] == "timezone" {
		posGiven = true
		if len(args) >= 2 {
			tz = args[1]
			rest = args[2:]
		} else {
			rest = args[1:]
		}
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if !posGiven {
		tz = *tzFlag
	}

	req := map[string]string{"timezone": strings.TrimSpace(tz)}
	var dto cliCronConfigDTO
	if err := f.client().DoScoped(context.Background(), "PUT", "/cron/config", req, &dto); err != nil {
		fmt.Fprintf(stderr, "cron config set: %v\n", err)
		return 1
	}
	if dto.Timezone == "" {
		fmt.Fprintln(stdout, "cron timezone cleared (now UTC)")
	} else {
		fmt.Fprintf(stdout, "cron timezone set to %s\n", dto.Timezone)
	}
	return 0
}
