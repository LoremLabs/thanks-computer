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
		if st, err := c.GetStack(ctx, name); err == nil && st != nil && st.ActiveVersion != nil {
			remoteN = *st.ActiveVersion
			d.Remote = fmt.Sprintf("v%d", remoteN)
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
	var red, green, yellow, bold, reset string
	if tty {
		red = "\x1b[31m"
		green = "\x1b[32m"
		yellow = "\x1b[33m"
		bold = "\x1b[1m"
		reset = "\x1b[0m"
	}

	// Column widths from plain-text values; ANSI is added after
	// padding so the escape codes don't throw off the math.
	var nameW, remoteW, localW int
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
	}

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
		fmt.Fprintf(w, "%s%s%s%s%s%s  remote=%s  local=%s  %s→ %s%s\n",
			markerC, marker, reset,
			bold, padRight(d.Stack, nameW), reset,
			padRight(d.Remote, remoteW),
			padRight(d.Local, localW),
			noteC, d.Note, reset)
	}
	return any
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
