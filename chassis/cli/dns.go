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
	case "config":
		return runDNSConfig(args[1:], stdout, stderr)
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
  zone verify <origin>   Verify the zone's NS delegate to us, then activate it
  zone list              List your delegated zones
  zone delete <origin>   Revoke a delegated zone
  record add <origin>    Add an override/extra record to a zone
  record list <origin>   List a zone's override records
  record rm <origin>     Revoke a zone's override record
  config show            Show the chassis DNS synthesis config (NS/edge/MX/TTL)
  config set [flags]     Set the chassis DNS synthesis config (no restart)
  render [<zone>]        Print the zone(s) the chassis would serve

Run 'txco dns <subcommand> --help' for flags.
`)
}

// splitComma splits a comma list, trimming blanks.
func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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
	t.Tenant = resolveTenant(*f.tenant, effectiveProfile(*f.target, *f.profile))
	return client.New(t)
}

// confirm guards a mutating dns command: shows the target and prompts (or
// fails closed) before modifying a non-local chassis, unless assumeYes.
func (f dnsFlags) confirm(assumeYes bool, stderr io.Writer) error {
	label := resolveTargetLabel(".", *f.target, *f.addr, *f.profile)
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	tenant := resolveTenant(*f.tenant, effectiveProfile(*f.target, *f.profile))
	return confirmMutation(label, t.Addr, tenant, assumeYes, false, stderr)
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
	case "verify":
		return runDNSZoneVerify(args[1:], stdout, stderr)
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
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
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
	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "dns zone create: %v\n", err)
		return 1
	}
	res, err := f.client().CreateZone(context.Background(), origin, strings.TrimSpace(*mode))
	if err != nil {
		fmt.Fprintf(stderr, "dns zone create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created zone %s (mode=%s)\n\n", res.Zone.Origin, res.Zone.Mode)
	fmt.Fprintln(stdout, res.Delegation)
	if res.Warning != "" {
		fmt.Fprintf(stderr, "\nWARNING: %s\n", res.Warning)
	}
	return 0
}

func runDNSZoneVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns zone verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns zone verify [flags] <origin>\n\nConfirm the zone's NS resolve to the chassis nameservers, then activate it\n(needed only when --dns-require-zone-verification is on).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	origin, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if origin == "" {
		fmt.Fprintln(stderr, "dns zone verify: <origin> is required (e.g. ops.example.com)")
		return 2
	}
	res, err := f.client().VerifyZone(context.Background(), origin)
	if err != nil {
		fmt.Fprintf(stderr, "dns zone verify: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "verified %s — the zone is now live.\n", res.Origin)
	if res.Warning != "" {
		fmt.Fprintf(stderr, "\nWARNING: %s\n", res.Warning)
	}
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
	fmt.Fprintln(tw, "ORIGIN\tMODE\tSTATUS\tNS\tTTL")
	for _, z := range zones {
		status := "verified"
		if z.VerifiedAt == "" {
			status = "pending"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", z.Origin, z.Mode, status, z.MName, z.DefaultTTL)
	}
	_ = tw.Flush()
	return 0
}

func runDNSZoneDelete(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns zone delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
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
	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "dns zone delete: %v\n", err)
		return 1
	}
	if err := f.client().RevokeZone(context.Background(), origin); err != nil {
		fmt.Fprintf(stderr, "dns zone delete: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked zone %s\n", origin)
	return 0
}

// --- config -----------------------------------------------------------

func runDNSConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "dns config: expected show|set")
		return 2
	}
	switch args[0] {
	case "show", "get":
		return runDNSConfigShow(args[1:], stdout, stderr)
	case "set":
		return runDNSConfigSet(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dns config: unknown subcommand %q\n", args[0])
		return 2
	}
}

func printDNSConfig(w io.Writer, cfg *client.DNSConfig) {
	src := "boot --dns-* flags (no settings row yet)"
	if cfg.Configured {
		src = "operator-set (dns config set)"
	}
	fmt.Fprintf(w, "nameservers: %s\n", strings.Join(cfg.Nameservers, ", "))
	fmt.Fprintf(w, "edge-ips:    %s\n", strings.Join(cfg.EdgeIPs, ", "))
	fmt.Fprintf(w, "mx-host:     %s\n", cfg.MXHost)
	fmt.Fprintf(w, "mx-priority: %d\n", cfg.MXPriority)
	fmt.Fprintf(w, "ttl:         %d\n", cfg.TTL)
	fmt.Fprintf(w, "source:      %s\n", src)
}

func runDNSConfigShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := f.client().GetDNSConfig(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "dns config show: %v\n", err)
		return 1
	}
	printDNSConfig(stdout, cfg)
	return 0
}

func runDNSConfigSet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dns config set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerDNSFlags(fs)
	ns := fs.String("nameservers", "", "comma-separated authoritative nameserver FQDNs customers delegate to")
	edge := fs.String("edge-ips", "", "comma-separated A/AAAA target IP(s) for apex + per-stack hosts")
	mx := fs.String("mx", "", "mail exchanger hostname (the LMTP head's public name)")
	mxpri := fs.Int("mx-priority", 10, "MX preference value")
	ttl := fs.Int("ttl", 60, "synthesized record TTL in seconds")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco dns config set [flags]\n\nOnly the flags you pass are changed. Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	if !set["nameservers"] && !set["edge-ips"] && !set["mx"] && !set["mx-priority"] && !set["ttl"] {
		fmt.Fprintln(stderr, "dns config set: provide at least one of --nameservers/--edge-ips/--mx/--mx-priority/--ttl")
		return 2
	}
	var patch client.DNSConfigPatch
	if set["nameservers"] {
		v := splitComma(*ns)
		patch.Nameservers = &v
	}
	if set["edge-ips"] {
		v := splitComma(*edge)
		patch.EdgeIPs = &v
	}
	if set["mx"] {
		v := strings.TrimSpace(*mx)
		patch.MXHost = &v
	}
	if set["mx-priority"] {
		v := *mxpri
		patch.MXPriority = &v
	}
	if set["ttl"] {
		v := *ttl
		patch.TTL = &v
	}
	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "dns config set: %v\n", err)
		return 1
	}
	cfg, err := f.client().PutDNSConfig(context.Background(), patch)
	if err != nil {
		fmt.Fprintf(stderr, "dns config set: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "updated DNS synthesis config:")
	printDNSConfig(stdout, cfg)
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
	rtype := fs.String("type", "", "record type: NS|A|AAAA|MX|TXT|CNAME")
	rdata := fs.String("rdata", "", "record data in zone-file form (e.g. '10 mail.example.com.' for MX)")
	ttl := fs.Int64("ttl", -1, "record TTL in seconds (default: inherit zone default)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
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
	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "dns record add: %v\n", err)
		return 1
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
	rtype := fs.String("type", "", "record type: NS|A|AAAA|MX|TXT|CNAME")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
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
	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "dns record rm: %v\n", err)
		return 1
	}
	if err := f.client().RevokeRecord(context.Background(), origin, *name, *rtype); err != nil {
		fmt.Fprintf(stderr, "dns record rm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s %s in %s\n", *name, strings.ToUpper(*rtype), origin)
	return 0
}
