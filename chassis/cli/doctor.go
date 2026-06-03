package cli

// `txco doctor` is the setup-diagnostics command: one sectioned report that
// eagerly runs the normally-lazy preflight paths (so the silently-swallowed
// signer/target errors become visible) and prints the RESOLVED state — which
// profile, which key backend, which chassis, signed vs unsigned, version
// sync. It is diagnose-only: it reports findings and the exact fix command,
// and never mutates local state. Modeled on `claude doctor`: section headers
// with ├/└ children, mixing plain `key: value` state lines with ✓/⚠/✗ checks.
//
// Scope is local + remote: local sections always run; the Chassis/Updates
// sections make one best-effort round-trip (skipped under --offline). Exit is
// non-zero iff any check is ✗ (a ⚠ does not fail), so it is CI-friendly.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/cloud"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
	"github.com/loremlabs/thanks-computer/chassis/cli/update"
)

// doctorStatus is the verdict glyph class for one report line.
type doctorStatus int

const (
	statusInfo doctorStatus = iota // plain state line, no judgement
	statusOK                       // ✓ healthy
	statusWarn                     // ⚠ degraded / advisory (does NOT fail the run)
	statusFail                     // ✗ broken (sets a non-zero exit)
)

// finding is one line under a section. Label+Value render as "label: value"
// (or just one when the other is empty); Hint is the fix suggestion shown on
// ⚠/✗ lines.
type finding struct {
	Status doctorStatus
	Label  string
	Value  string
	Hint   string
}

type section struct {
	Title    string
	Findings []finding
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	var profile, addr string
	var offline, jsonOut bool
	fs.StringVar(&profile, "profile", "", "signing profile to inspect (default: active profile)")
	fs.StringVar(&addr, "addr", "", "chassis admin endpoint (overrides the resolved target)")
	fs.BoolVar(&offline, "offline", false, "skip network checks (chassis reachability + updates)")
	fs.BoolVar(&jsonOut, "json", false, "emit machine-readable JSON instead of the text report")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	secs := []section{
		diagnosticsSection(),
		homeProfileSection(profile),
		signingSection(profile),
	}

	var srv update.ServerInfo
	var srvOK bool
	if offline {
		secs = append(secs, section{Title: "Chassis", Findings: []finding{
			{Status: statusInfo, Label: "checks", Value: "skipped (--offline)"},
		}})
	} else {
		var ch section
		ch, srv, srvOK = chassisSection(profile, addr)
		secs = append(secs, ch)
	}
	secs = append(secs, updatesSection(srv, srvOK, offline))

	if cs, ok := cloudSection(); ok {
		secs = append(secs, cs)
	}

	if jsonOut {
		if err := renderDoctorJSON(stdout, secs); err != nil {
			fmt.Fprintf(stderr, "txco: doctor: %v\n", err)
			return 1
		}
	} else {
		renderDoctorText(stdout, secs)
	}
	if anyFail(secs) {
		return 1
	}
	return 0
}

// --- sections -------------------------------------------------------------

// diagnosticsSection reports the build identity + self-update posture.
func diagnosticsSection() section {
	version, commit := buildVersionCommit()
	stamped := version != "" && commit != "" && commit != "dev"

	fs := []finding{
		{Status: statusInfo, Label: "version", Value: displayVersion(version)},
		{Status: statusInfo, Label: "commit", Value: shortCommit(commit)},
		{Status: statusInfo, Label: "platform", Value: runtime.GOOS + "/" + runtime.GOARCH},
		{Status: statusInfo, Label: "go", Value: runtime.Version()},
	}
	if Build.BuildTimestamp != "" {
		fs = append(fs, finding{Status: statusInfo, Label: "built", Value: shortTimestamp(Build.BuildTimestamp)})
	}
	if Build.Chassis != "" {
		fs = append(fs, finding{Status: statusInfo, Label: "chassis", Value: Build.Chassis})
	}

	m := update.ResolveCurrent(Build.InstallMethod)
	fs = append(fs, finding{Status: statusInfo, Label: "install", Value: m.Name})
	if !stamped {
		fs = append(fs, finding{
			Status: statusWarn, Label: "build", Value: "unstamped (self-update disabled, version checks limited)",
			Hint: "use a release build to enable `txco upgrade`",
		})
	} else if m.SelfUpdate {
		fs = append(fs, finding{Status: statusOK, Label: "self-update", Value: "available (`txco upgrade`)"})
	} else {
		fs = append(fs, finding{Status: statusInfo, Label: "self-update", Value: "delegated (" + m.Name + ")"})
	}
	return section{Title: "Diagnostics", Findings: fs}
}

// homeProfileSection reports $TXCO_HOME and the resolved profile + how it
// resolved.
func homeProfileSection(profileFlag string) section {
	var fs []finding

	home, err := auth.HomePath()
	if err != nil {
		fs = append(fs, finding{Status: statusFail, Label: "home", Value: err.Error(),
			Hint: "set TXCO_HOME to a writable directory"})
		return section{Title: "Home & profile", Findings: fs}
	}
	fs = append(fs, finding{Status: statusOK, Label: "home", Value: home})
	if fi, serr := os.Stat(home); serr == nil && fi.Mode().Perm()&0o077 != 0 {
		fs = append(fs, finding{Status: statusWarn, Label: "permissions",
			Value: fmt.Sprintf("%#o (want 0700 — keys live here)", fi.Mode().Perm()),
			Hint:  fmt.Sprintf("chmod 700 %s", home)})
	}

	name, rerr := auth.ResolveProfile(profileFlag)
	if rerr != nil {
		fs = append(fs, finding{Status: statusFail, Label: "profile", Value: rerr.Error()})
		return section{Title: "Home & profile", Findings: fs}
	}
	switch name {
	case auth.ActiveNone:
		fs = append(fs, finding{Status: statusWarn, Label: "profile",
			Value: "none (logged out — requests sent unsigned)",
			Hint:  "`txco auth login` or pass --profile"})
	default:
		fs = append(fs, finding{Status: statusInfo, Label: "profile",
			Value: fmt.Sprintf("%s (via %s)", name, profileSource(profileFlag, home))})
	}
	return section{Title: "Home & profile", Findings: fs}
}

// signingSection is the eager-check win: it loads the signer up front so the
// errors loadSigner normally swallows (missing key, dead agent, bad perms)
// surface as actionable findings instead of a later confusing 401.
func signingSection(profileFlag string) section {
	var fs []finding

	name, err := auth.ResolveProfile(profileFlag)
	if err != nil {
		return section{Title: "Signing", Findings: []finding{{Status: statusFail, Label: "profile", Value: err.Error()}}}
	}
	if name == auth.ActiveNone {
		return section{Title: "Signing", Findings: []finding{{
			Status: statusWarn, Label: "signing", Value: "disabled (logged out)",
			Hint: "requests sent unsigned; `txco auth login` to sign",
		}}}
	}

	// Eagerly resolve the backend — this is the path loadSigner silences.
	s, lerr := auth.LoadSignerForActiveProfile(profileFlag)
	if lerr != nil {
		fs = append(fs, classifySignerError(lerr))
		return section{Title: "Signing", Findings: fs}
	}
	if s == nil {
		return section{Title: "Signing", Findings: []finding{{
			Status: statusWarn, Label: "signing", Value: "not configured (no key for this profile)",
			Hint: "`txco auth bootstrap-local --secret <s>` or `txco auth init`",
		}}}
	}

	fs = append(fs, finding{Status: statusOK, Label: "signing", Value: "ready"})
	fs = append(fs, finding{Status: statusInfo, Label: "key_id", Value: s.KeyID()})

	// Meta colours in the backend + bound chassis, and lets us check key-file
	// perms for file-backed profiles.
	if metaPath, merr := auth.MetaPath(name); merr == nil {
		if m, lmErr := auth.LoadMeta(metaPath); lmErr == nil && m != nil {
			fs = append(fs, finding{Status: statusInfo, Label: "backend", Value: m.EffectiveKeySource()})
			if m.ChassisURL != "" {
				fs = append(fs, finding{Status: statusInfo, Label: "bound chassis", Value: m.ChassisURL})
			}
			if m.EffectiveKeySource() == auth.SourceFile {
				kp := m.KeyPath
				if kp == "" {
					kp = strings.TrimSuffix(metaPath, ".meta.json")
				}
				if fi, serr := os.Stat(kp); serr == nil && fi.Mode().Perm()&0o077 != 0 {
					fs = append(fs, finding{Status: statusWarn, Label: "key permissions",
						Value: fmt.Sprintf("%#o (want 0600)", fi.Mode().Perm()),
						Hint:  fmt.Sprintf("chmod 600 %s", kp)})
				}
			}
		}
	}
	return section{Title: "Signing", Findings: fs}
}

// classifySignerError turns a signer-load failure into a precise finding with
// the matching fix command, using the package's typed sentinels.
func classifySignerError(err error) finding {
	switch {
	case errors.Is(err, signer.ErrNoAgent):
		return finding{Status: statusFail, Label: "ssh-agent", Value: "unavailable (SSH_AUTH_SOCK unset or unreachable)",
			Hint: "start ssh-agent and `ssh-add <your key>`"}
	case errors.Is(err, signer.ErrKeyNotInAgent):
		return finding{Status: statusFail, Label: "ssh-agent", Value: "reachable, but the enrolled key isn't loaded",
			Hint: "`ssh-add <your key>` (or `ssh-add -l` to see what's loaded)"}
	}
	var pme *signer.PassphraseMissingError
	if errors.As(err, &pme) {
		return finding{Status: statusFail, Label: "key", Value: "encrypted; no passphrase available non-interactively",
			Hint: "load it into ssh-agent: `ssh-add " + pme.Path + "`"}
	}
	return finding{Status: statusFail, Label: "signing", Value: err.Error(),
		Hint: "re-run `txco auth bootstrap-local` / `txco auth enroll`"}
}

// chassisSection resolves the admin target, reaches /healthz, and (signed)
// whoami. Returns the fetched ServerInfo so updatesSection can reuse it
// without a second round-trip.
func chassisSection(profileFlag, addr string) (section, update.ServerInfo, bool) {
	var fs []finding
	target := resolveTarget("", "", addr, "", "", profileFlag)
	if target.Addr == "" {
		fs = append(fs, finding{Status: statusWarn, Label: "chassis", Value: "no target configured",
			Hint: "pass --addr, set a txco.yaml target, or `txco login` to bind one"})
		return section{Title: "Chassis", Findings: fs}, update.ServerInfo{}, false
	}
	fs = append(fs, finding{Status: statusInfo, Label: "target",
		Value: target.Addr + " (via " + targetSource(addr, profileFlag) + ")"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := update.FetchServerInfo(ctx, target.Addr, userAgent())
	srvOK := err == nil
	if err != nil {
		fs = append(fs, finding{Status: statusFail, Label: "reachable", Value: "no (" + err.Error() + ")",
			Hint: "is a chassis running/serving admin at that address?"})
	} else {
		fs = append(fs, finding{Status: statusOK, Label: "reachable", Value: "yes"})
		fs = append(fs, finding{Status: statusInfo, Label: "server", Value: serverBuildSummary(info)})
	}

	// Signed identity check — confirms the whole signing chain end to end.
	who, werr := client.New(target).Whoami(ctx)
	switch {
	case werr != nil:
		fs = append(fs, finding{Status: statusWarn, Label: "whoami", Value: werr.Error(),
			Hint: "if the chassis requires signing, fix the Signing section above"})
	default:
		fs = append(fs, finding{Status: statusOK, Label: "identity", Value: whoamiSummary(who)})
		if len(who.Capabilities) > 0 {
			fs = append(fs, finding{Status: statusInfo, Label: "capabilities", Value: strings.Join(who.Capabilities, ", ")})
		}
	}
	return section{Title: "Chassis", Findings: fs}, info, srvOK
}

// updatesSection reports self-update posture, CLI-vs-server version policy
// (reusing the ServerInfo from chassisSection), and the latest GitHub release.
func updatesSection(srv update.ServerInfo, srvOK, offline bool) section {
	var fs []finding

	// Server policy (from the /healthz fetch chassisSection already did).
	if srvOK && srv.Client != nil {
		if notice := update.OutdatedNotice(Build.Version, srv.Client); notice != "" {
			st := statusWarn
			if srv.Client.Critical {
				st = statusFail
			}
			fs = append(fs, finding{Status: st, Label: "server policy", Value: notice,
				Hint: "`txco upgrade`"})
		} else {
			fs = append(fs, finding{Status: statusOK, Label: "server policy", Value: "in sync with server's minimum"})
		}
	}

	if offline {
		fs = append(fs, finding{Status: statusInfo, Label: "release check", Value: "skipped (--offline)"})
		return section{Title: "Updates", Findings: fs}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := update.Check(ctx, Build.Version, userAgent())
	switch {
	case err != nil:
		fs = append(fs, finding{Status: statusWarn, Label: "release check", Value: err.Error()})
	case !res.Comparable:
		fs = append(fs, finding{Status: statusInfo, Label: "latest release", Value: "v" + res.Latest + " (this build has no comparable version)"})
	case res.Available:
		fs = append(fs, finding{Status: statusWarn, Label: "latest release", Value: "v" + res.Latest + " available",
			Hint: "`txco upgrade`"})
	default:
		fs = append(fs, finding{Status: statusOK, Label: "latest release", Value: "up to date (v" + res.Current + ")"})
	}
	return section{Title: "Updates", Findings: fs}
}

// cloudSection reports stored cloud tokens, if any. Returns ok=false (omit the
// section) when no cloud token files exist.
func cloudSection() (section, bool) {
	home, err := auth.HomePath()
	if err != nil {
		return section{}, false
	}
	entries, err := os.ReadDir(filepath.Join(home, "cloud"))
	if err != nil || len(entries) == 0 {
		return section{}, false
	}
	var fs []finding
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		prof := strings.TrimSuffix(e.Name(), ".json")
		t, lerr := cloud.LoadCloudToken(prof)
		if lerr != nil {
			fs = append(fs, finding{Status: statusWarn, Label: prof, Value: "unreadable token (" + lerr.Error() + ")",
				Hint: "`txco login`"})
			continue
		}
		who := t.Subject
		if who == "" {
			who = t.Email
		}
		dest := t.CloudURL
		if dest != "" {
			dest = " @ " + dest
		}
		if t.Expired(now) {
			fs = append(fs, finding{Status: statusWarn, Label: prof, Value: "session expired" + " (" + who + ")",
				Hint: "`txco login` to refresh"})
		} else {
			fs = append(fs, finding{Status: statusOK, Label: prof, Value: who + dest})
		}
	}
	if len(fs) == 0 {
		return section{}, false
	}
	return section{Title: "Cloud", Findings: fs}, true
}

// --- helpers --------------------------------------------------------------

// profileSource explains how the active profile name resolved, mirroring
// auth.ResolveProfile's precedence chain (flag → env → active file → default).
func profileSource(flag, home string) string {
	if flag != "" {
		return "--profile"
	}
	if os.Getenv("TXCO_PROFILE") != "" {
		return "TXCO_PROFILE env"
	}
	if _, err := os.Stat(filepath.Join(home, "active")); err == nil {
		return "active file"
	}
	return "default"
}

// targetSource explains how the chassis address resolved, mirroring
// resolveTarget's precedence (flag → env → profile chassis_url → txco.yaml/default).
func targetSource(addr, profileFlag string) string {
	if addr != "" {
		return "--addr"
	}
	if os.Getenv("TXCO_ADMIN_ADDR") != "" {
		return "TXCO_ADMIN_ADDR env"
	}
	if auth.ProfileChassisURL(profileFlag) != "" {
		return "profile chassis_url"
	}
	return "txco.yaml/default"
}

func whoamiSummary(w *client.WhoamiResponse) string {
	parts := []string{w.Source}
	if w.ActorID != "" {
		parts = append(parts, w.ActorID)
	}
	if w.SuperAdmin {
		parts = append(parts, "super-admin")
	}
	return strings.Join(parts, " · ")
}

func shortCommit(c string) string {
	if c == "" || c == "dev" {
		return "(unstamped)"
	}
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func shortTimestamp(ts string) string {
	if len(ts) >= 10 && ts[4] == '-' && ts[7] == '-' {
		return ts[:10]
	}
	return ts
}

func anyFail(secs []section) bool {
	for _, s := range secs {
		for _, f := range s.Findings {
			if f.Status == statusFail {
				return true
			}
		}
	}
	return false
}

// --- rendering ------------------------------------------------------------

func renderDoctorText(w io.Writer, secs []section) {
	c := newColorizer(w)
	for i, s := range secs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, c.title(s.Title))
		for j, f := range s.Findings {
			conn := "├ "
			if j == len(s.Findings)-1 {
				conn = "└ "
			}
			fmt.Fprintf(w, "%s%s\n", c.dim(conn), f.line(c))
		}
	}
}

func (f finding) line(c colorizer) string {
	var b strings.Builder
	switch f.Status {
	case statusOK:
		b.WriteString(c.green("✓ "))
	case statusWarn:
		b.WriteString(c.yellow("⚠ "))
	case statusFail:
		b.WriteString(c.red("✗ "))
	}
	switch {
	case f.Label != "" && f.Value != "":
		b.WriteString(f.Label + ": " + f.Value)
	case f.Label != "":
		b.WriteString(f.Label)
	default:
		b.WriteString(f.Value)
	}
	if f.Hint != "" && (f.Status == statusWarn || f.Status == statusFail) {
		b.WriteString(c.dim("  → " + f.Hint))
	}
	return b.String()
}

type colorizer struct{ on bool }

func newColorizer(w io.Writer) colorizer { return colorizer{on: banner.IsTTY(w)} }

func (c colorizer) wrap(code, s string) string {
	if !c.on {
		return s
	}
	return code + s + "\x1b[0m"
}

func (c colorizer) green(s string) string  { return c.wrap("\x1b[32m", s) }
func (c colorizer) yellow(s string) string { return c.wrap("\x1b[33m", s) }
func (c colorizer) red(s string) string    { return c.wrap("\x1b[31m", s) }
func (c colorizer) dim(s string) string    { return c.wrap("\x1b[2m", s) }
func (c colorizer) title(s string) string  { return c.wrap("\x1b[1m\x1b[36m", s) }

// renderDoctorJSON emits the same findings as machine-readable JSON.
func renderDoctorJSON(w io.Writer, secs []section) error {
	type findingJSON struct {
		Status string `json:"status"`
		Label  string `json:"label,omitempty"`
		Value  string `json:"value,omitempty"`
		Hint   string `json:"hint,omitempty"`
	}
	type sectionJSON struct {
		Title    string        `json:"title"`
		Findings []findingJSON `json:"findings"`
	}
	statusStr := map[doctorStatus]string{
		statusInfo: "info", statusOK: "ok", statusWarn: "warn", statusFail: "fail",
	}
	out := make([]sectionJSON, 0, len(secs))
	for _, s := range secs {
		sj := sectionJSON{Title: s.Title}
		for _, f := range s.Findings {
			sj.Findings = append(sj.Findings, findingJSON{
				Status: statusStr[f.Status], Label: f.Label, Value: f.Value, Hint: f.Hint,
			})
		}
		out = append(out, sj)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
