package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
)

// runDiff compares the local OPS/ tree against the running chassis's view.
// v1 reports adds and changes; deletes are out of scope (apply is upsert-only,
// so removing a local file doesn't remove the rule on the server).
//
// `op://NAME` references are resolved using the selected target's operations
// map before comparison, so the local rules' txcl matches what the server
// stores.
func runDiff(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("diff", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON ({stacks, files}) instead of the text report")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco diff [flags] [<dir>] [<target>]

Compare local <dir>/OPS/ against a chassis admin endpoint. Reports rules
that would be added or changed by 'txco apply'. <dir> defaults to ".".

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dirArg, targetArg := splitDirTarget(fs.Args())
	if targetArg != "" && tf.Target == "" {
		tf.Target = targetArg
	}
	dir, err := workspaceDir(dirArg)
	if err != nil {
		fmt.Fprintf(stderr, "diff: resolve dir: %v\n", err)
		return 1
	}

	localOps, err := bundle.Walk(dir)
	if err != nil {
		fmt.Fprintf(stderr, "diff: walk %s: %v\n", dir, err)
		return 1
	}

	// Resolve op://NAME references locally so the comparison sees the
	// same shape the chassis stores. Apply mock-strip too, so a `diff`
	// preview matches what `apply --target prod` would push.
	resolved := resolveFullTarget(dir, tf.Target)
	resolverOps := buildOpRefMap(resolved)
	for i, op := range localOps {
		if !oprefs.HasRefs(op.Txcl) {
			continue
		}
		substituted, err := oprefs.ResolveOpRefs(op.Txcl, resolverOps)
		if err != nil {
			fmt.Fprintf(stderr, "diff: %s (%s/%d/%s): %v\n",
				op.SourcePath, op.Stack, op.Scope, op.Name, err)
			return 1
		}
		localOps[i].Txcl = substituted
	}
	if resolved.Mock == "deny" {
		for i := range localOps {
			localOps[i].MockRes = ""
		}
	}

	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)
	ctx := context.Background()
	remoteOps, err := c.ListOps(ctx, "")
	if err != nil {
		fmt.Fprintf(stderr, "diff: list remote: %v\n", err)
		return 1
	}

	// Stack-level version drift report. Surfaces the case where local
	// content matches remote content but the version *pointers* have
	// drifted (e.g. admin UI activated an older version, or local was
	// never `txco pull`-ed so .txco/<stack>.state.json is missing).
	// Without this, "no changes" is misleading on a chassis that's
	// been edited out-of-band.
	remoteStackNames := uniqueStackNamesFromOps(remoteOps)
	drifts := buildDrifts(ctx, c, dir, localOps, remoteStackNames)
	if len(drifts) > 0 {
		decorateStackURLs(ctx, c, drifts)
	}
	anyDivergent := false
	for _, d := range drifts {
		if d.Divergent {
			anyDivergent = true
			break
		}
	}
	if anyDivergent && !*jsonOut {
		fmt.Fprintln(stdout, "stacks:")
		printDriftTable(stdout, drifts)
		fmt.Fprintln(stdout)
	}

	type key struct {
		Stack string
		Scope int
		Name  string
	}
	remoteByKey := make(map[key]client.Op, len(remoteOps))
	for _, op := range remoteOps {
		remoteByKey[key{op.Stack, op.Scope, op.Name}] = op
	}

	type change struct {
		stack    string
		scope    int
		name     string
		kind     string // "add" or "change"
		localOp  bundle.Op
		remoteOp client.Op
	}
	var changes []change

	for _, op := range localOps {
		k := key{op.Stack, op.Scope, op.Name}
		if r, ok := remoteByKey[k]; ok {
			if r.Txcl != op.Txcl || r.MockReq != op.MockReq || r.MockRes != op.MockRes {
				changes = append(changes, change{op.Stack, op.Scope, op.Name, "change", op, r})
			}
		} else {
			changes = append(changes, change{op.Stack, op.Scope, op.Name, "add", op, client.Op{}})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].stack != changes[j].stack {
			return changes[i].stack < changes[j].stack
		}
		if changes[i].scope != changes[j].scope {
			return changes[i].scope < changes[j].scope
		}
		return changes[i].name < changes[j].name
	})

	if *jsonOut {
		files := make([]diffFileChange, 0, len(changes))
		for _, ch := range changes {
			files = append(files, diffFileChange{Stack: ch.stack, Scope: ch.scope, Name: ch.name, Kind: ch.kind})
		}
		return emitDiffJSON(stdout, stderr, drifts, files)
	}

	if len(changes) == 0 {
		if anyDivergent {
			fmt.Fprintln(stdout, "no per-file content changes; version pointers differ — see stacks above.")
		} else {
			fmt.Fprintln(stdout, "no changes")
		}
		return 0
	}

	fmt.Fprintln(stdout, "files:")
	for _, c := range changes {
		switch c.kind {
		case "add":
			fmt.Fprintf(stdout, "  + %s/%d/%s\n", c.stack, c.scope, c.name)
		case "change":
			fmt.Fprintf(stdout, "  ~ %s/%d/%s\n", c.stack, c.scope, c.name)
		}
	}
	fmt.Fprintf(stdout, "\n%d change(s); run 'txco apply' to push.\n", len(changes))
	return 0
}

// diffFileChange is the JSON form of one pending rule add/change.
type diffFileChange struct {
	Stack string `json:"stack"`
	Scope int    `json:"scope"`
	Name  string `json:"name"`
	Kind  string `json:"kind"` // "add" | "change"
}

// emitDiffJSON writes the diff as a single `{stacks, files}` object. Unlike
// the text form (which prints the stacks block only when something diverged),
// the JSON always includes every stack so consumers get a stable shape. Diff
// is a preview, not a probe — exit stays 0 on success regardless of pending
// changes; only an encode failure is non-zero.
func emitDiffJSON(stdout, stderr io.Writer, drifts []stackDrift, files []diffFileChange) int {
	if files == nil {
		files = []diffFileChange{}
	}
	payload := struct {
		Stacks []stackDriftJSON `json:"stacks"`
		Files  []diffFileChange `json:"files"`
	}{driftsToJSON(drifts), files}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(stderr, "diff: encode json: %v\n", err)
		return 1
	}
	return 0
}
