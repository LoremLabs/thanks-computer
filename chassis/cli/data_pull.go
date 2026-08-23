package cli

// `txco data pull` + the drift helpers behind `txco data apply`'s refusal.
//
// Blob seeding follows the git model: the tree is your working copy, the
// chassis index is the remote. A runtime blob/put that repoints a seeded
// name is the remote moving past your last push — `data apply` refuses to
// overwrite it (non-fast-forward), `data pull` brings the live content into
// BLOBS/ so the next apply ships it, and `--force` makes the tree win.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// hasBlobRows reports whether a collected pack set carries any BLOBS/ row.
func hasBlobRows(files []client.StackFile) bool {
	for _, f := range files {
		if storeseed.IsBlobPath(f.Path) {
			return true
		}
	}
	return false
}

// driftedStackBlobs returns the seeded names the chassis changed at runtime
// that the local tree does NOT already carry — git's non-fast-forward test.
// A name whose live content equals the local file (you pulled, or you made
// the same edit) is a fast-forward and passes; a drifted name the tree
// dropped is a conflict (you deleted what the remote edited). localHashes
// maps blob name → sha256 of the local BLOBS/ file.
func driftedStackBlobs(rows []client.StackBlob, localHashes map[string]string) []client.StackBlob {
	var out []client.StackBlob
	for _, r := range rows {
		if !r.Drifted {
			continue
		}
		if localHashes[r.Name] == r.SHA256 {
			continue // tree already holds the live content — fast-forward
		}
		out = append(out, r)
	}
	return out
}

// localBlobHashes indexes a collected pack set's BLOBS/ rows by blob name.
func localBlobHashes(files []client.StackFile) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		if storeseed.IsBlobPath(f.Path) {
			out[storeseed.PackName(f.Path)] = f.ContentHash
		}
	}
	return out
}

// runDataPull materialises each stack's LIVE seeded blobs into
// OPS/<stack>/BLOBS/<name>: the bytes a runtime put moved, hash-verified on
// the way down, written only when the local file differs. After a pull the
// tree matches the chassis, so `txco data apply` ships it and drift clears.
func runDataPull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("data pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	timeout := fs.Duration("timeout", 5*time.Minute, "per-request timeout (raise for large blobs)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco data pull [flags] [<dir>]

Bring the live BLOBS/ of every stack under <dir>/OPS/ into the tree: each
seeded blob name's current content is streamed down (hash-verified) and
written to OPS/<stack>/BLOBS/<name> when it differs from the local file.
This is how you resolve a `+"`txco data apply`"+` refusal — pull, review the
diff, apply. (VECTORS/ + KV/ materialisation: coming soon.)

<dir> defaults to ".".

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir, err := workspaceDir(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "data pull: resolve dir: %v\n", err)
		return 1
	}
	ops, diags, err := bundle.WalkDiag(dir)
	if err != nil {
		fmt.Fprintf(stderr, "data pull: walk %s: %v\n", dir, err)
		return 1
	}
	if len(diags) > 0 {
		for _, d := range diags {
			fmt.Fprintf(stderr, "data pull: %s\n", d.Msg)
		}
		return 1
	}
	c := f.clientWithTimeout(*timeout)
	ctx := context.Background()

	type result struct {
		Stack   string   `json:"stack"`
		Seeded  int      `json:"seeded"`
		Updated []string `json:"updated"`
	}
	var results []result
	rc := 0
	for _, stack := range sortedKeys(groupOpsByStack(ops)) {
		live, lerr := c.ListStackBlobs(ctx, stack)
		if lerr != nil {
			fmt.Fprintf(stderr, "data pull: %s: list seeded blobs: %v\n", stack, lerr)
			rc = 1
			continue
		}
		res := result{Stack: stack, Seeded: len(live), Updated: []string{}}
		stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
		for _, row := range live {
			dest := filepath.Join(stackDir, storeseed.DirBlobs, filepath.FromSlash(row.Name))
			if h, _, herr := hashFileStreaming(dest); herr == nil && h == row.SHA256 {
				continue // local file already IS the live content
			}
			if derr := downloadBlobToFile(ctx, c, row.SHA256, dest); derr != nil {
				fmt.Fprintf(stderr, "data pull: %s: %s: %v\n", stack, row.Name, derr)
				rc = 1
				continue
			}
			res.Updated = append(res.Updated, row.Name)
		}
		results = append(results, res)
		if !*f.jsonOut {
			fmt.Fprintf(stdout, "%s — %d seeded blob%s, %d updated\n", stack, res.Seeded, pluralS(res.Seeded), len(res.Updated))
			for _, n := range res.Updated {
				fmt.Fprintf(stdout, "    %s\n", filepath.ToSlash(filepath.Join(storeseed.DirBlobs, n)))
			}
		}
	}
	if *f.jsonOut {
		_ = emitJSON(stdout, stderr, results)
	} else if len(results) == 0 {
		fmt.Fprintln(stdout, "no stacks found under "+filepath.Join(dir, "OPS"))
	}
	return rc
}
