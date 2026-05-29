package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
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
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml (default: the config's `target:` field, or `dev`)")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides target's chassis URL)")
	user := fs.String("user", "", "basic auth user (overrides target's user)")
	pass := fs.String("pass", "", "basic auth password (overrides target's pass)")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", auth.HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug for the chassis (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco diff [flags] [<dir>]

Compare local <dir>/OPS/ against a chassis admin endpoint. Reports rules
that would be added or changed by 'txco apply'. <dir> defaults to ".".

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := resolveDir(fs.Arg(0))
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
	resolved := resolveFullTarget(dir, *target)
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

	clientTarget := resolveTarget(dir, *target, *addr, *user, *pass, *profile)
	clientTarget.Tenant = resolveTenant(*tenant, *profile)
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
	anyDivergent := false
	for _, d := range drifts {
		if d.Divergent {
			anyDivergent = true
			break
		}
	}
	if anyDivergent {
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

