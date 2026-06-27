package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// runData routes `txco data <subcommand> ...` — the declarative store-seed
// surface: push/pull the VECTORS/ + KV/ packs between the local tree and the
// runtime stores, and inspect/tear down what's there. Distinct from `txco
// apply`, which deploys CODE only (rules + FILES) and never touches data — data
// is opt-in. See chassis/storeseed.
func runData(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printDataUsage(stdout)
		return 0
	}
	switch args[0] {
	case "apply", "push":
		return runDataApply(args[1:], stdout, stderr)
	case "pull":
		fmt.Fprintln(stderr, "data pull: not yet implemented (materialise store → local packs)")
		return 1
	case "ls", "list":
		return runVectorLs(args[1:], stdout, stderr)
	case "show", "describe":
		return runVectorShow(args[1:], stdout, stderr)
	case "diff":
		return runVectorDiff(args[1:], stdout, stderr)
	case "rm", "drop", "delete":
		return runVectorRm(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printDataUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "data: unknown subcommand %q\n\n", args[0])
		printDataUsage(stderr)
		return 2
	}
}

func printDataUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage: txco data <subcommand> ...

Deploy + inspect declarative store-seed packs (VECTORS/, KV/). Data is opt-in:
`+"`txco apply`"+` deploys code only; data moves through these verbs.

Subcommands:
  apply [<dir>]            Deploy the local VECTORS/+KV/ packs (code carried forward, then reconciled)
  pull [<dir>]             Materialise the live store into local packs (coming soon)
  ls                       List the tenant's vector collections (model/dims/count)
  show <collection>        Show a collection's pin + item IDs
  diff <collection> <pack> Compare a local VECTORS/*.jsonl pack to the live store
  rm <collection>          Drop a whole collection (explicit teardown; apply never does this)

Run 'txco data <subcommand> --help' for flags.
`)
}

// vectorFlags bundles the common target/auth flags + the client builder
// (mirrors dnsFlags).
type vectorFlags struct {
	target, addr, user, pass, profile, tenant *string
	jsonOut                                   *bool
}

func registerVectorFlags(fs *flag.FlagSet) vectorFlags {
	return vectorFlags{
		target:  fs.String("target", "", "target name from txco.yaml"),
		addr:    fs.String("addr", "", "chassis admin endpoint"),
		user:    fs.String("user", "", "basic auth user"),
		pass:    fs.String("pass", "", "basic auth password"),
		profile: fs.String("profile", "", fmt.Sprintf("signing profile (defaults to TXCO_PROFILE, then %s/active)", auth.HomePathPretty())),
		tenant:  fs.String("tenant", "", "tenant slug"),
		jsonOut: fs.Bool("json", false, "emit JSON instead of a table"),
	}
}

func (f vectorFlags) client() *client.Client {
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	t.Tenant = resolveTenant(*f.tenant, *f.profile)
	return client.New(t)
}

func (f vectorFlags) clientWithTimeout(d time.Duration) *client.Client {
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	t.Tenant = resolveTenant(*f.tenant, *f.profile)
	return client.NewWithTimeout(t, d)
}

// confirm guards a mutating data command: shows the target and prompts (or
// fails closed) before modifying a non-local chassis, unless assumeYes. See
// confirmMutation.
func (f vectorFlags) confirm(assumeYes bool, stderr io.Writer) error {
	resolved := resolveFullTarget(".", *f.target)
	t := resolveTarget(".", *f.target, *f.addr, *f.user, *f.pass, *f.profile)
	return confirmMutation(resolved.Name, t.Addr, assumeYes, *f.jsonOut, stderr)
}

// --- ls ---------------------------------------------------------------

func runVectorLs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vector ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cols, err := f.client().VectorListCollections(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "vector ls: %v\n", err)
		return 1
	}
	if *f.jsonOut {
		return emitJSON(stdout, stderr, cols)
	}
	if len(cols) == 0 {
		fmt.Fprintln(stdout, "no collections")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "COLLECTION\tMODEL\tDIMS\tMETRIC\tITEMS")
	for _, c := range cols {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\n", c.Name, dashIfEmpty(c.EmbeddingModel), c.Dimensions, c.Metric, c.Count)
	}
	_ = tw.Flush()
	return 0
}

// --- show -------------------------------------------------------------

func runVectorShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vector show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	showIDs := fs.Bool("ids", false, "print every item ID (default: a sample + count)")
	name, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if name == "" {
		fmt.Fprintln(stderr, "vector show: a <collection> is required")
		return 2
	}
	d, err := f.client().VectorGetCollection(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "vector show: %v\n", err)
		return 1
	}
	if *f.jsonOut {
		return emitJSON(stdout, stderr, d)
	}
	fmt.Fprintf(stdout, "collection:  %s\n", d.Name)
	fmt.Fprintf(stdout, "model:       %s\n", dashIfEmpty(d.EmbeddingModel))
	fmt.Fprintf(stdout, "dimensions:  %d\n", d.Dimensions)
	fmt.Fprintf(stdout, "metric:      %s\n", d.Metric)
	fmt.Fprintf(stdout, "items:       %d\n", d.Count)
	ids := append([]string(nil), d.IDs...)
	sort.Strings(ids)
	if *showIDs {
		for _, id := range ids {
			fmt.Fprintln(stdout, "  "+id)
		}
	} else if len(ids) > 0 {
		sample := ids
		if len(sample) > 10 {
			sample = sample[:10]
		}
		fmt.Fprintf(stdout, "sample:      %v", sample)
		if len(ids) > len(sample) {
			fmt.Fprintf(stdout, " … (+%d more; --ids for all)", len(ids)-len(sample))
		}
		fmt.Fprintln(stdout)
	}
	return 0
}

// --- diff -------------------------------------------------------------

func runVectorDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vector diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "vector diff: usage: txco vector diff <collection> <pack.jsonl>")
		return 2
	}
	name, packPath := fs.Arg(0), fs.Arg(1)
	// Re-parse so flags may follow the two positionals.
	if err := fs.Parse(fs.Args()[2:]); err != nil {
		return 2
	}

	packIDs, err := packItemIDs(packPath)
	if err != nil {
		fmt.Fprintf(stderr, "vector diff: read pack: %v\n", err)
		return 1
	}

	d, err := f.client().VectorGetCollection(context.Background(), name)
	storeIDs := map[string]struct{}{}
	switch {
	case err == nil:
		for _, id := range d.IDs {
			storeIDs[id] = struct{}{}
		}
	default:
		// A missing collection means a first apply — everything is "added".
		fmt.Fprintf(stderr, "(collection %q not in store yet: %v)\n", name, err)
	}

	var added, removed []string
	for id := range packIDs {
		if _, ok := storeIDs[id]; !ok {
			added = append(added, id)
		}
	}
	for id := range storeIDs {
		if _, ok := packIDs[id]; !ok {
			removed = append(removed, id)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	common := len(packIDs) - len(added)

	if *f.jsonOut {
		return emitJSON(stdout, stderr, map[string]any{
			"collection": name, "added": added, "removed": removed, "unchanged_ids": common,
		})
	}
	fmt.Fprintf(stdout, "apply would change collection %q:\n", name)
	fmt.Fprintf(stdout, "  + %d added, - %d removed, = %d in both\n", len(added), len(removed), common)
	for _, id := range added {
		fmt.Fprintf(stdout, "  + %s\n", id)
	}
	for _, id := range removed {
		fmt.Fprintf(stdout, "  - %s\n", id)
	}
	return 0
}

// --- rm ---------------------------------------------------------------

func runVectorRm(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("vector rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	name, ok := parsePositional(fs, args)
	if !ok {
		return 2
	}
	if name == "" {
		fmt.Fprintln(stderr, "vector rm: a <collection> is required")
		return 2
	}
	if !*yes {
		fmt.Fprintf(stderr, "vector rm: pass --yes to drop the whole collection %q and all its items\n", name)
		return 1
	}
	removed, err := f.client().VectorDropCollection(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "vector rm: %v\n", err)
		return 1
	}
	if *f.jsonOut {
		return emitJSON(stdout, stderr, map[string]any{"collection": name, "removed_items": removed})
	}
	fmt.Fprintf(stdout, "dropped %q (%d items removed)\n", name, removed)
	return 0
}

// --- helpers ----------------------------------------------------------

func emitJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(stderr, "encode json: %v\n", err)
		return 1
	}
	return 0
}

// packItemIDs reads a VECTORS pack (NDJSON) and returns the set of item IDs.
func packItemIDs(path string) (map[string]struct{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Reuse the pack line-splitter; the path may be any local file, so derive
	// a nominal pack name only to satisfy NewRawPack (unused here).
	pk := storeseed.RawPack{Bytes: raw}
	out := map[string]struct{}{}
	for i, line := range pk.Lines() {
		var it struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(line, &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if it.ID == "" {
			return nil, fmt.Errorf("line %d: missing id", i+1)
		}
		out[it.ID] = struct{}{}
	}
	return out, nil
}

// --- data apply -------------------------------------------------------

// runDataApply deploys the local VECTORS/+KV/ packs for every stack that has
// them. Each pack push creates a new stack version that REPLACES the data
// category and carries the active version's code forward (manage="data"), then
// activates it — which reconciles the changed packs into the runtime stores.
// `txco apply` (code) never touches data, so this is the deliberate, separate
// "update the catalog" step.
func runDataApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("data apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	f := registerVectorFlags(fs)
	timeout := fs.Duration("timeout", 5*time.Minute, "per-request timeout (raise for large packs)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco data apply [flags] [<dir>]

Deploy the local VECTORS/+KV/ store-seed packs for every stack under <dir>/OPS/.
Each stack's code is carried forward from its active version; only the data is
replaced, then reconciled into the runtime stores. The stack must already have
an active version (deploy code first with `+"`txco apply`"+`).

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
		fmt.Fprintf(stderr, "data apply: resolve dir: %v\n", err)
		return 1
	}
	ops, diags, err := bundle.WalkDiag(dir)
	if err != nil {
		fmt.Fprintf(stderr, "data apply: walk %s: %v\n", dir, err)
		return 1
	}
	if len(diags) > 0 {
		for _, d := range diags {
			fmt.Fprintf(stderr, "data apply: %s\n", d.Msg)
		}
		return 1
	}

	if err := f.confirm(*yes, stderr); err != nil {
		fmt.Fprintf(stderr, "data apply: %v\n", err)
		return 1
	}
	c := f.clientWithTimeout(*timeout)
	ctx := context.Background()

	type result struct {
		Stack   string `json:"stack"`
		Version int64  `json:"version,omitempty"`
		Packs   int    `json:"packs"`
		Skipped bool   `json:"skipped,omitempty"`
		Reason  string `json:"reason,omitempty"`
	}
	var results []result
	rc := 0

	for _, stack := range sortedKeys(groupOpsByStack(ops)) {
		packs, perr := collectStorePacks(filepath.Join(dir, "OPS", filepath.FromSlash(stack)))
		if perr != nil {
			fmt.Fprintf(stderr, "data apply: %s: collect packs: %v\n", stack, perr)
			rc = 1
			continue
		}
		if len(packs) == 0 {
			continue // no data packs in this stack
		}

		// Require an active version to carry code forward from.
		rec, gerr := c.GetStack(ctx, stack)
		if gerr != nil || rec == nil || rec.ActiveVersion == nil {
			results = append(results, result{Stack: stack, Packs: len(packs), Skipped: true,
				Reason: "no active version — deploy code first with `txco apply`"})
			if !*f.jsonOut {
				fmt.Fprintf(stderr, "data apply: %s skipped — no active version; run `txco apply` first\n", stack)
			}
			rc = 1
			continue
		}

		version, derr := c.CreateDraft(ctx, stack, "active")
		if derr != nil {
			fmt.Fprintf(stderr, "data apply: %s: create draft: %v\n", stack, derr)
			rc = 1
			continue
		}
		if _, err := c.PutDraftFilesScoped(ctx, stack, version, packs, "data"); err != nil {
			fmt.Fprintf(stderr, "data apply: %s: upload packs for v%d: %v\n", stack, version, err)
			rc = 1
			continue
		}
		act, aerr := c.Activate(ctx, stack, version)
		if aerr != nil {
			fmt.Fprintf(stderr, "data apply: %s: activate v%d: %v\n", stack, version, aerr)
			rc = 1
			continue
		}
		results = append(results, result{Stack: stack, Version: act.VersionNumber, Packs: len(packs)})
		if !*f.jsonOut {
			fmt.Fprintf(stdout, "%s v%d — %d data pack%s reconciled\n",
				stack, act.VersionNumber, len(packs), pluralS(len(packs)))
		}
	}

	if *f.jsonOut {
		_ = emitJSON(stdout, stderr, results)
	} else if len(results) == 0 {
		fmt.Fprintln(stdout, "no data packs found (VECTORS/ or KV/) under any stack")
	}
	return rc
}
