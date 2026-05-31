package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// runDNS routes `txco dns <subcommand> ...`.
func runDNS(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printDNSUsage(stdout)
		return 0
	}
	switch args[0] {
	case "render":
		return runDNSRender(args[1:], stdout, stderr)
	case "zone":
		return runDNSZone(args[1:], stdout, stderr)
	case "record":
		return runDNSRecord(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printDNSUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "dns: unknown subcommand %q\n\n", args[0])
		printDNSUsage(stderr)
		return 2
	}
}

func printDNSUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco dns <subcommand> ...

Subcommands:
  zone create <origin>   Register a delegated zone (prints NS delegation steps)
  zone list              List your delegated zones
  zone delete <origin>   Revoke a delegated zone
  record add <origin>    Add an override/extra record to a zone
  record list <origin>   List a zone's override records
  record rm <origin>     Revoke a zone's override record
  render [<zone>]        Print the zone(s) the chassis would serve

Run 'txco dns <subcommand> --help' for flags.
`)
}

// dnsFlags bundles the common target/auth flags every dns subcommand
// accepts, plus the helper that builds an authenticated client.
type dnsFlags struct {
	target, addr, user, pass, profile, tenant *string
}

func registerDNSFlags(fs *flag.FlagSet) dnsFlags {
	return dnsFlags{
		target:  fs.String("target", "", "target name from txco.yaml"),
		addr:    fs.String("addr", "", "chassis admin endpoint"),
		user:    fs.String("user", "", "basic auth user"),
		pass:    fs.String("pass", "", "basic auth password"),
		profile: fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty())),
		tenant:  fs.String("tenant", "", "tenant slug"),
	}
}

func (f dnsFlags) client() *client.Client {
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	t.Tenant = resolveTenant(*f.tenant, *f.profile)
	return client.New(t)
}

// parsePositional parses args, then (if a leading positional is present)
// re-parses the remaining args so flags may follow the positional —
// e.g. `zone create ops.example.com --mode manual`.
func parsePositional(fs *flag.FlagSet, args []string) (string, bool) {
	if err := fs.Parse(args); err != nil {
		return "", false
	}
	pos := fs.Arg(0)
	if pos != "" {
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return "", false
		}
	}
	return pos, true
}

// --- render -----------------------------------------------------------

func runDNSRender(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	zone := fs.String("zone", "", "render only this origin (default: all the tenant's zones)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns render [flags] [<zone>]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	pos, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	z := *zone
	if z == "" {
		z = pos
	}
	out, err := f.client().GetDNSRender(context.Background(), z)
	if err != nil {
		fmt.Fprintf(stderr, "dns render: %v\n", err)
		return 1
	}
	if strings.TrimSpace(out) == "" {
		fmt.Fprintln(stderr, "dns render: no zones served for this tenant")
		return 0
	}
	fmt.Fprint(stdout, out)
	if !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(stdout)
	}
	return 0
}

// --- zone -------------------------------------------------------------

func runDNSZone(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dns zone: expected create|list|delete")
		return 2
	}
	switch args[0] {
	case "create":
		return runDNSZoneCreate(args[1:], stdout, stderr)
	case "list", "ls":
		return runDNSZoneList(args[1:], stdout, stderr)
	case "delete", "rm", "revoke":
		return runDNSZoneDelete(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dns zone: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runDNSZoneCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns zone create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	mode := fs.String("mode", "", "zone mode: pattern (default, synthesized) | manual (materialized-only)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns zone create [flags] <origin>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" {
		fmt.Fprintln(stderr, "dns zone create: <origin> is required (e.g. ops.example.com)")
		return 2
	}
	res, err := f.client().CreateZone(context.Background(), origin, strings.TrimSpace(*mode))
	if err != nil {
		fmt.Fprintf(stderr, "dns zone create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created zone %s (mode=%s)\n\n", res.Zone.Origin, res.Zone.Mode)
	fmt.Fprintln(stdout, res.Delegation)
	return 0
}

func runDNSZoneList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns zone list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	zones, err := f.client().ListZones(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "dns zone list: %v\n", err)
		return 1
	}
	if len(zones) == 0 {
		fmt.Fprintln(stdout, "no delegated zones")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ORIGIN\tMODE\tNS\tTTL")
	for _, z := range zones {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", z.Origin, z.Mode, z.MName, z.DefaultTTL)
	}
	_ = tw.Flush()
	return 0
}

func runDNSZoneDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns zone delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns zone delete [flags] <origin>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" {
		fmt.Fprintln(stderr, "dns zone delete: <origin> is required")
		return 2
	}
	if err := f.client().RevokeZone(context.Background(), origin); err != nil {
		fmt.Fprintf(stderr, "dns zone delete: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked zone %s\n", origin)
	return 0
}

// --- record -----------------------------------------------------------

func runDNSRecord(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dns record: expected add|list|rm")
		return 2
	}
	switch args[0] {
	case "add", "create":
		return runDNSRecordAdd(args[1:], stdout, stderr)
	case "list", "ls":
		return runDNSRecordList(args[1:], stdout, stderr)
	case "rm", "delete", "revoke":
		return runDNSRecordRm(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dns record: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runDNSRecordAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns record add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	name := fs.String("name", "@", "record name (relative label, or @ for apex)")
	rtype := fs.String("type", "", "record type: NS|A|AAAA|MX|TXT")
	rdata := fs.String("rdata", "", "record data in zone-file form (e.g. '10 mail.example.com.' for MX)")
	ttl := fs.Int64("ttl", -1, "record TTL in seconds (default: inherit zone default)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns record add [flags] <origin> --type <T> --rdata <V>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" || strings.TrimSpace(*rtype) == "" || strings.TrimSpace(*rdata) == "" {
		fmt.Fprintln(stderr, "dns record add: <origin>, --type and --rdata are required")
		return 2
	}
	if err := f.client().CreateRecord(context.Background(), origin, *name, *rtype, *ttl, *rdata); err != nil {
		fmt.Fprintf(stderr, "dns record add: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "added %s %s %s in %s\n", *name, strings.ToUpper(*rtype), *rdata, origin)
	return 0
}

func runDNSRecordList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns record list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" {
		fmt.Fprintln(stderr, "dns record list: <origin> is required")
		return 2
	}
	recs, err := f.client().ListRecords(context.Background(), origin)
	if err != nil {
		fmt.Fprintf(stderr, "dns record list: %v\n", err)
		return 1
	}
	if len(recs) == 0 {
		fmt.Fprintln(stdout, "no override records")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tTTL\tRDATA")
	for _, r := range recs {
		ttl := "(zone)"
		if r.TTL != nil {
			ttl = fmt.Sprintf("%d", *r.TTL)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Type, ttl, r.Rdata)
	}
	_ = tw.Flush()
	return 0
}

func runDNSRecordRm(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns record rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	name := fs.String("name", "@", "record name (relative label, or @ for apex)")
	rtype := fs.String("type", "", "record type: NS|A|AAAA|MX|TXT")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns record rm [flags] <origin> --type <T>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" || strings.TrimSpace(*rtype) == "" {
		fmt.Fprintln(stderr, "dns record rm: <origin> and --type are required")
		return 2
	}
	if err := f.client().RevokeRecord(context.Background(), origin, *name, *rtype); err != nil {
		fmt.Fprintf(stderr, "dns record rm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s %s in %s\n", *name, strings.ToUpper(*rtype), origin)
	return 0
}
