package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

// stackDrift summarizes the local vs. chassis view of one stack — the
// shape `txco diff` and `txco status` both render. We compute one of
// these per (stack) and let the caller decide how to format.
type stackDrift struct {
	Stack     string
	Remote    string // "v12" or "—" if no active
	Local     string // "v14 (clean)" / "v14 (edited since pull)" / "untracked"
	Note      string // "in sync" / "chassis ahead by 2 …" / "chassis rolled back N …" / "no local state recorded — run `txco pull <stack>`"
	URL       string // reachable stack URL, e.g. "https://app.acme.com" (empty when none/unknown)
	DevURL    string // reachable URL on the local `txco dev` chassis (empty when no dev run is known)
	MailPath  string // inbound mail pattern for _mail stacks, e.g. "*@acme.com" (empty otherwise)
	Headless  bool   // stack opted out of the auto-minted routing URL (`txco stack set --no-host`)
	Divergent bool
}

// buildDrifts collects per-stack drift records for the union of local
// and remote stack names. `localOps` may be nil when the caller only
// cares about remote-side state.
func buildDrifts(ctx context.Context, c *client.Client, dir string, localOps []bundle.Op, remoteStacks []string) []stackDrift {
	stackSet := map[string]bool{}
	for _, op := range localOps {
		stackSet[op.Stack] = true
	}
	for _, name := range remoteStacks {
		stackSet[name] = true
	}
	names := make([]string, 0, len(stackSet))
	for s := range stackSet {
		names = append(names, s)
	}
	sort.Strings(names)

	out := make([]stackDrift, 0, len(names))
	for _, name := range names {
		d := stackDrift{Stack: name, Remote: "—", Local: "untracked"}

		// Remote pointer.
		var remoteN int64 = -1
		if st, err := c.GetStack(ctx, name); err == nil && st != nil {
			if st.ActiveVersion != nil {
				remoteN = *st.ActiveVersion
				d.Remote = fmt.Sprintf("v%d", remoteN)
			}
			// nil = older server that doesn't report the field → leave unflagged.
			if st.MintHostname != nil && !*st.MintHostname {
				d.Headless = true
			}
		}

		// Local state + cleanliness.
		saved, _ := state.Load(dir, name)
		if saved != nil {
			d.Local = fmt.Sprintf("v%d", saved.VersionNumber)
			// Prefer reading from disk so we pick up any local edits
			// the caller may not have plumbed through localOps.
			stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(name))
			files, ferr := loadLocalStackFiles(stackDir)
			if ferr == nil && saved.ManifestHash != "" {
				if localManifestHash(files) == saved.ManifestHash {
					d.Local += " (clean)"
				} else {
					d.Local += " (edited since pull)"
				}
			}
		}

		// Categorize.
		switch {
		case saved == nil && remoteN >= 0:
			d.Note = fmt.Sprintf("no local state recorded — run `txco pull %s`", name)
			d.Divergent = true
		case saved == nil && remoteN < 0:
			d.Note = "no local state, no remote active"
			d.Divergent = true
		case remoteN < 0:
			d.Note = "remote has no active version"
			d.Divergent = true
		case remoteN == saved.VersionNumber:
			d.Note = "in sync"
		case remoteN > saved.VersionNumber:
			d.Note = fmt.Sprintf("chassis ahead by %d — `txco pull %s --force` to sync local",
				remoteN-saved.VersionNumber, name)
			d.Divergent = true
		default: // remoteN < saved.VersionNumber
			d.Note = fmt.Sprintf("chassis rolled back %d versions — local mirrors a newer version",
				saved.VersionNumber-remoteN)
			d.Divergent = true
		}

		out = append(out, d)
	}
	return out
}

// decorateStackURLs best-effort annotates each drift with a reachable URL
// for its stack, resolved from the chassis's tenant_hostnames. It is a
// nicety so `txco status` can hand the user a clickable address: any error
// (older chassis with no /hostnames, none bound) leaves URLs empty and the
// table renders exactly as before.
func decorateStackURLs(ctx context.Context, c *client.Client, drifts []stackDrift) {
	hosts, err := c.ListHostnames(ctx, false)
	if err != nil {
		return
	}
	// best[stack] = the preferred hostname bound to that stack. Key "" holds
	// the best mail-only hostname (h.Stack==""), used for the bare _mail
	// stack's inbound path — it is never used as an HTTP URL (guarded below).
	best := map[string]client.Hostname{}
	for _, h := range hosts {
		if h.RevokedAt != "" {
			continue // revoked — not usable
		}
		if cur, ok := best[h.Stack]; !ok || betterHostname(h, cur) {
			best[h.Stack] = h
		}
	}
	for i := range drifts {
		if h, ok := best[drifts[i].Stack]; ok && drifts[i].Stack != "" {
			drifts[i].URL = "https://" + h.Hostname
		}
		// Inbound mail path for _mail-type stacks. Strategy-B routing sends
		// *@<verified-domain> to <hostname.stack>/_mail (nested) or, for a
		// mail-only domain (h.Stack==""), to <tenant>/_mail. So a stack
		// "X/_mail" is reachable at *@<host> for hostnames bound to "X"; the
		// bare "_mail" stack catches the mail-only domains.
		if parent, isMail := mailParent(drifts[i].Stack); isMail {
			if h, ok := best[parent]; ok {
				drifts[i].MailPath = "*@" + h.Hostname
			}
		}
	}
}

// mailParent reports whether stack is a _mail-type stack and, if so, the
// HTTP/parent stack whose verified hostnames deliver mail to it.
// "web/_mail" → "web"; the bare "_mail" → "" (the tenant's mail-only
// domains). Anything else → ("", false).
func mailParent(stack string) (parent string, isMail bool) {
	if stack == "_mail" {
		return "", true
	}
	if strings.HasSuffix(stack, "/_mail") {
		return strings.TrimSuffix(stack, "/_mail"), true
	}
	return "", false
}

// betterHostname reports whether a should be preferred over b as the
// display URL for a stack: a verified hostname (one that actually routes)
// beats an unverified one, and among equals the shorter name wins — a
// custom domain like app.acme.com over the auto-minted
// <stack>-<rand>.<origin>. Hostname is the final, stable tiebreak.
func betterHostname(a, b client.Hostname) bool {
	av, bv := a.VerifiedAt != "", b.VerifiedAt != ""
	if av != bv {
		return av
	}
	if len(a.Hostname) != len(b.Hostname) {
		return len(a.Hostname) < len(b.Hostname)
	}
	return a.Hostname < b.Hostname
}

// uniqueStackNamesFromOps returns the sorted, distinct stack names
// observed in a remote `ListOps` response. Used to seed buildDrifts'
// remote side without redoing the dedupe at every call site.
func uniqueStackNamesFromOps(ops []client.Op) []string {
	seen := map[string]bool{}
	for _, op := range ops {
		seen[op.Stack] = true
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// stackDriftJSON is the machine-readable form of a stackDrift, shared by
// `txco status --json` and `txco diff --json` so both commands speak the
// same stack schema. `url` is omitted when no reachable hostname is bound.
type stackDriftJSON struct {
	Stack     string `json:"stack"`
	Remote    string `json:"remote"`
	Local     string `json:"local"`
	URL       string `json:"url,omitempty"`
	DevURL    string `json:"dev_url,omitempty"`
	MailPath  string `json:"mail_path,omitempty"`
	Note      string `json:"note"`
	Divergent bool   `json:"divergent"`
}

// driftsToJSON converts the internal drift records into their wire form.
func driftsToJSON(drifts []stackDrift) []stackDriftJSON {
	out := make([]stackDriftJSON, 0, len(drifts))
	for _, d := range drifts {
		out = append(out, stackDriftJSON{
			Stack: d.Stack, Remote: d.Remote, Local: d.Local,
			URL: d.URL, DevURL: d.DevURL, MailPath: d.MailPath,
			Note: d.Note, Divergent: d.Divergent,
		})
	}
	return out
}

// printDriftTable renders a drift list with the same shape as
// `txco diff`'s stacks: section. Columns are padded to the widest
// value in each so output lines up like git status. ANSI color is
// applied when w is a TTY:
//
//	red `!`        — divergent marker
//	bold stack     — stack name
//	yellow note    — divergent note (chassis ahead / rolled back / no state)
//	green note     — in-sync note
//
// Returns true if any row was divergent.
func printDriftTable(w io.Writer, drifts []stackDrift) bool {
	tty := banner.IsTTY(w)
	var red, green, yellow, cyan, bold, reset string
	if tty {
		red = "\x1b[31m"
		green = "\x1b[32m"
		yellow = "\x1b[33m"
		cyan = "\x1b[36m"
		bold = "\x1b[1m"
		reset = "\x1b[0m"
	}

	// Column widths from plain-text values; ANSI is added after
	// padding so the escape codes don't throw off the math. The url=
	// column only appears when at least one stack has a resolved URL, so
	// hostname-less setups (and `txco diff`, which never sets URL) render
	// exactly as before.
	var nameW, remoteW, localW, urlCellW int
	for _, d := range drifts {
		if n := len(d.Stack); n > nameW {
			nameW = n
		}
		if n := len(d.Remote); n > remoteW {
			remoteW = n
		}
		if n := len(d.Local); n > localW {
			localW = n
		}
		if d.URL != "" {
			if n := len("url=") + len(d.URL); n > urlCellW {
				urlCellW = n
			}
		}
	}
	showURL := urlCellW > 0

	any := false
	for _, d := range drifts {
		marker := "  "
		markerC := ""
		noteC := green
		if d.Divergent {
			marker = "! "
			markerC = red
			noteC = yellow
			any = true
		}

		// Optional url= cell, kept width-aligned so the note column lines
		// up whether or not a given row has a URL. The bare https:// stays
		// intact for terminal auto-linking.
		urlSeg := ""
		if showURL {
			plain, colored := "", ""
			if d.URL != "" {
				plain = "url=" + d.URL
				colored = "url=" + cyan + d.URL + reset
			}
			pad := urlCellW - len(plain)
			if pad < 0 {
				pad = 0
			}
			urlSeg = "  " + colored + strings.Repeat(" ", pad)
		}

		// Trailing dev= segment: the reachable URL on the local `txco dev`
		// chassis (from .txco/dev/urls.json). Appended after the note,
		// unaligned, so it never disturbs the column math and is absent on
		// non-dev/`txco diff` rows.
		devSeg := ""
		if d.DevURL != "" {
			devSeg = "  dev=" + cyan + d.DevURL + reset
		}
		// Trailing mail= segment: the inbound *@host pattern for _mail
		// stacks. Like dev=, it's appended after the note (unaligned), so
		// it never disturbs the column math.
		mailSeg := ""
		if d.MailPath != "" {
			mailSeg = "  mail=" + cyan + d.MailPath + reset
		}
		// Trailing headless marker: the stack opted out of the auto-minted
		// routing URL, so the url= cell is (correctly) blank — say so
		// explicitly rather than leaving the absence ambiguous.
		headlessSeg := ""
		if d.Headless {
			headlessSeg = "  " + yellow + "headless" + reset
		}

		fmt.Fprintf(w, "%s%s%s%s%s%s  remote=%s  local=%s%s  %s→ %s%s%s%s%s\n",
			markerC, marker, reset,
			bold, padRight(d.Stack, nameW), reset,
			padRight(d.Remote, remoteW),
			padRight(d.Local, localW),
			urlSeg,
			noteC, d.Note, reset, headlessSeg, devSeg, mailSeg)
	}
	return any
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
