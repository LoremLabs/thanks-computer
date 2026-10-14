package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

// runSync records which chassis version each local stack is, from content:
// the fast-forward a new machine (or a stale workspace) needs before it can
// push, without a pull rewriting the tree. It builds every stack exactly as
// `apply` would, compares the build with the active version's per-file
// hashes (and the recorded baseline's), and saves what it found in
// .txco/<stack>.state.json. It writes no workspace files and nothing on the
// chassis.
//
//	txco sync [--stack S] [--adopt] [<dir>] [<target>]
func runSync(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("sync", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := applyOpts{targetFlags: bindTargetFlags(fs), sync: true}
	stack := fs.String("stack", "", "check one stack only")
	fs.StringArrayVar(&opts.skip, "skip", nil, "skip any stack whose name contains this substring (repeatable)")
	fs.BoolVar(&opts.adopt, "adopt", false, "for a stack with no baseline that differs from the chassis, record the active version anyway: this tree is that version plus your edits")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable JSON (one object per stack)")
	fs.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "per-request timeout for chassis calls")
	fs.BoolVar(&opts.noSource, "no-source", false, "compare as `apply --no-source` deploys (no compute sources)")
	verbose := fs.Bool("verbose", false, "trace every HTTP request/response to stderr (TXCO_VERBOSE=1)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco sync [flags] [<dir>] [<target>]

Record which chassis version each local stack is, so this workspace can push
— on a new machine, or after someone deployed from elsewhere. Each stack is
built as `+"`txco apply`"+` would build it and compared, file by file, with the
active version (and with the version this workspace last recorded):

  in sync        the tree is the active version; recorded
  v5 → v7        the tree already has every change since v5 (a git pull
                 brought them) and its own edits touch none; recorded v7
  conflict       the chassis changed files since the recorded version that
                 this tree differs on; nothing recorded
  behind         the tree is an older version exactly; that one is recorded
  no baseline    nothing recorded and the tree differs from the active
                 version; --adopt records it anyway

Writes only .txco/ — never the OPS/ tree, never the chassis. `+"`txco push`"+` and
`+"`txco apply`"+` run the same check themselves when the recorded version is stale.
Exit code is 1 when any stack is in conflict, behind, or has no baseline.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *verbose {
		_ = os.Setenv("TXCO_VERBOSE", "1")
	}
	dirArg, targetArg := splitDirTarget(fs.Args())
	if targetArg != "" && opts.Target == "" {
		opts.Target = targetArg
	}
	dir, err := workspaceDir(dirArg)
	if err != nil {
		fmt.Fprintf(stderr, "sync: resolve dir: %v\n", err)
		return 1
	}
	ws, err := readWorkspace(dir)
	if err != nil {
		fmt.Fprintf(stderr, "sync: %v\n", err)
		return 1
	}
	if len(ws.StackNames()) == 0 {
		fmt.Fprintf(stderr, "sync: no stacks found — expected an OPS/ tree, or txco.yaml stacks: bindings, at or above %s.\n", dir)
		return 1
	}
	return applyOps("sync", ws, opts, *stack, stdout, stderr)
}

// syncRow is one stack's sync outcome, also its --json form.
type syncRow struct {
	Stack string `json:"stack"`
	// Verdict: in_sync, forwarded, local_edits, adopted, conflict, behind,
	// no_baseline, not_on_chassis, no_active_version or error.
	Verdict    string   `json:"verdict"`
	Active     int64    `json:"active,omitempty"`
	Before     int64    `json:"before,omitempty"` // the recorded baseline before the sync (0 = none)
	After      int64    `json:"after,omitempty"`  // the baseline after it (0 = none)
	LocalEdits int      `json:"local_edits"`
	Paths      []string `json:"paths,omitempty"` // the conflicting, changed or differing files
	Error      string   `json:"error,omitempty"`
}

// needsAttention reports whether a row makes `txco sync` exit 1.
func (r syncRow) needsAttention() bool {
	switch r.Verdict {
	case "conflict", "behind", "no_baseline", "error":
		return true
	}
	return false
}

// syncStack runs the content check for one stack and records what it found.
func syncStack(ctx context.Context, c *client.Client, ws *localWorkspace, key, stack string, build *stackBuild, ops []bundle.Op, adopt bool) syncRow {
	row := syncRow{Stack: stack}
	saved, _ := state.Load(ws.Dir, stack, key)
	if saved != nil {
		row.Before, row.After = saved.VersionNumber, saved.VersionNumber
	}
	rec, err := c.GetStack(ctx, stack)
	if err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == http.StatusNotFound {
			row.Verdict = "not_on_chassis"
			return row
		}
		row.Verdict, row.Error = "error", err.Error()
		return row
	}
	if rec.ActiveVersion == nil {
		row.Verdict = "no_active_version"
		return row
	}
	row.Active = *rec.ActiveVersion
	ff, err := fastForward(ctx, c, ws.Dir, stack, saved, rec, build)
	if err != nil {
		row.Verdict, row.Error = "error", err.Error()
		return row
	}
	row.LocalEdits = len(ff.Edits)
	switch ff.Kind {
	case ffInSync:
		recordInSync(ws, key, stack, ff.Active, build.Hash())
		row.Verdict, row.After = "in_sync", ff.Active
	case ffForward:
		row.Verdict = "local_edits"
		if saved == nil || saved.VersionNumber != ff.Active {
			recordBaseline(ws.Dir, key, stack, ff.Active, rec.CodeManifestHash)
			row.Verdict, row.After = "forwarded", ff.Active
		}
	case ffConflict:
		row.Verdict, row.Paths = "conflict", authorPaths(stack, ff.Conflicts, ops)
	case ffBehind:
		recordInSync(ws, key, stack, ff.Base, build.Hash())
		row.Verdict, row.After, row.Paths = "behind", ff.Base, authorPaths(stack, ff.Conflicts, ops)
	case ffNoBase:
		if adopt {
			recordBaseline(ws.Dir, key, stack, ff.Active, rec.CodeManifestHash)
			row.Verdict, row.After = "adopted", ff.Active
		} else {
			row.Verdict, row.Paths = "no_baseline", authorPaths(stack, ff.Edits, ops)
		}
	}
	return row
}

// reportSync prints the rows (or emits them as JSON) and returns the exit code.
func reportSync(cmd string, rows []syncRow, jsonOut bool, stdout, stderr io.Writer) int {
	rc := 0
	for _, r := range rows {
		if r.needsAttention() {
			rc = 1
		}
	}
	if jsonOut {
		if rows == nil {
			rows = []syncRow{}
		}
		if err := writeJSON(stdout, rows); err != nil {
			fmt.Fprintf(stderr, "%s: encode json: %v\n", cmd, err)
			return 1
		}
		return rc
	}
	green, red, dim, reset := statusColors(stdout)
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Verdict]++
		ok, bad := green+"✓"+reset, red+"✗"+reset
		switch r.Verdict {
		case "in_sync":
			note := ""
			if r.Before != r.After {
				note = " (recorded)"
			}
			fmt.Fprintf(stdout, "%s %s: in sync at v%d%s\n", ok, r.Stack, r.Active, note)
		case "forwarded":
			from := "no baseline"
			if r.Before != 0 {
				from = fmt.Sprintf("v%d", r.Before)
			}
			fmt.Fprintf(stdout, "%s %s: %s → v%d — this tree has the chassis's changes; %d local edit%s to push\n",
				ok, r.Stack, from, r.Active, r.LocalEdits, pluralS(r.LocalEdits))
		case "local_edits":
			fmt.Fprintf(stdout, "%s %s: at v%d with %d local edit%s to push\n", ok, r.Stack, r.Active, r.LocalEdits, pluralS(r.LocalEdits))
		case "adopted":
			fmt.Fprintf(stdout, "%s %s: v%d adopted as the baseline; %d local edit%s to push\n", ok, r.Stack, r.Active, r.LocalEdits, pluralS(r.LocalEdits))
		case "conflict":
			fmt.Fprintf(stdout, "%s %s: conflict — the chassis changed %d file%s since v%d (now v%d) that this tree differs on:\n%s",
				bad, r.Stack, len(r.Paths), pluralS(len(r.Paths)), r.Before, r.Active, indentList(r.Paths, 10))
		case "behind":
			fmt.Fprintf(stdout, "%s %s: behind — this tree is v%d exactly; the chassis is at v%d (%s):\n%s",
				bad, r.Stack, r.After, r.Active, filesDiffer(len(r.Paths)), indentList(r.Paths, 10))
		case "no_baseline":
			fmt.Fprintf(stdout, "%s %s: no baseline — this tree differs from v%d in %d file%s:\n%s    if that is this tree's own work on top of v%d: `txco sync --adopt --stack %s`\n",
				bad, r.Stack, r.Active, len(r.Paths), pluralS(len(r.Paths)), indentList(r.Paths, 10), r.Active, r.Stack)
		case "not_on_chassis":
			fmt.Fprintf(stdout, "%s· %s: not on the chassis — `txco push %s` creates it%s\n", dim, r.Stack, r.Stack, reset)
		case "no_active_version":
			fmt.Fprintf(stdout, "%s· %s: the chassis has no active version of it%s\n", dim, r.Stack, reset)
		case "error":
			fmt.Fprintf(stdout, "%s %s: couldn't check: %s\n", bad, r.Stack, r.Error)
		}
	}
	if len(rows) > 1 {
		recap := fmt.Sprintf("%d in sync", counts["in_sync"]+counts["local_edits"])
		for _, v := range []struct{ key, label string }{
			{"forwarded", "fast-forwarded"}, {"adopted", "adopted"}, {"conflict", "conflict"},
			{"behind", "behind"}, {"no_baseline", "no baseline"}, {"not_on_chassis", "not on the chassis"}, {"error", "error"},
		} {
			if counts[v.key] > 0 {
				recap += fmt.Sprintf(" · %d %s", counts[v.key], v.label)
			}
		}
		fmt.Fprintf(stdout, "%s: %s\n", cmd, recap)
	}
	return rc
}
