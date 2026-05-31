package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	computeapi "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// runApply walks <dir>/OPS/, validates each resonator's txcl client-side, and
// POSTs the bundle to the chassis admin endpoint of the selected target.
//
//	txco apply [--target NAME] [--addr URL] [...] [<dir>]
//
// When the workspace's txco.yaml declares targets and operations, the
// selected target's operations map is used to resolve `"op://NAME"`
// references in each resonator's txcl. When the target's `mock` policy is
// "deny", `mock_res` is stripped from the bundle before pushing
// (`mock_req` is preserved — it's documentation, not executable).
func runApply(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", "", "target name from txco.yaml (default: the config's `target:` field, or `dev`)")
	addr := fs.String("addr", "", "chassis admin endpoint (overrides target's chassis URL)")
	user := fs.String("user", "", "basic auth user (overrides target's user)")
	pass := fs.String("pass", "", "basic auth password (overrides target's pass)")
	profile := fs.String("profile", "", fmt.Sprintf("signing profile name (defaults to TXCO_PROFILE, then %s/active, then \"local\")", auth.HomePathPretty()))
	tenant := fs.String("tenant", "", "tenant slug for the chassis (defaults to TXCO_TENANT, then meta's default_tenant, then \"default\")")
	dryRun := fs.Bool("dry-run", false, "validate the bundle locally; do not POST")
	noValidate := fs.Bool("no-validate", false, "skip server-side validation before activate (push+activate even if the chassis flags errors)")
	verbose := fs.Bool("verbose", false, "trace every HTTP request/response (method, URL, status, error body) to stderr. Equivalent to TXCO_VERBOSE=1, which works for ANY txco command.")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco apply [flags] [<dir>]

Walk <dir>/OPS/ for *.txcl resonator files, resolve any "op://NAME" references
using the selected target's operations map, validate each resonator, and push
the bundle to that target's chassis admin endpoint.

<dir> defaults to ".".

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

	dir, err := resolveDir(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "apply: resolve dir: %v\n", err)
		return 1
	}
	// Git-style root discovery: if there's no OPS/ right here, walk up
	// to the workspace root so `txco apply` works from anywhere in the
	// tree (e.g. inside OPS/<stack>/100/). An explicit <dir> arg that
	// already contains OPS/ short-circuits this.
	if root := findWorkspaceRoot(dir); root != "" && root != dir {
		fmt.Fprintf(stderr, "apply: using workspace root %s (found OPS/ above %s)\n", root, dir)
		dir = root
	}

	ops, err := bundle.Walk(dir)
	if err != nil {
		fmt.Fprintf(stderr, "apply: walk %s: %v\n", dir, err)
		return 1
	}
	if len(ops) == 0 {
		fmt.Fprintf(stderr, "apply: no resonators found — expected an OPS/ tree at or above %s.\n"+
			"  Run `txco apply` from your workspace root (the dir containing OPS/), or pass it: `txco apply <dir>`.\n", dir)
		return 1
	}

	// Resolve the selected target so we know which operations map to use
	// and whether mock_res should be stripped.
	resolved := resolveFullTarget(dir, *target)
	urlMap := buildOpRefMap(resolved)

	// Substitute op://NAME per resonator: a colocated <resonatordir>/NAME.js compute wins
	// (built + uploaded), else the txco.yaml operations URL. Computes build
	// into .txco/compute and are uploaded after the client is constructed
	// (skipped on --dry-run).
	subOps, builtComputes, cerr := resolveOpRefsColocated(ops, urlMap, dir, stderr)
	if cerr != nil {
		fmt.Fprintf(stderr, "apply: %v\n", cerr)
		return 1
	}
	ops = subOps
	if len(builtComputes) > 0 {
		_, _ = ensureGitignored(dir, ".txco/")
	}

	// Client-side parse — fail fast before contacting the server.
	for _, op := range ops {
		if _, err := txcl.Resonator(op.Txcl); err != nil {
			fmt.Fprintf(stderr, "apply: parse error at %s (%s/%d/%s): %v\n",
				op.SourcePath, op.Stack, op.Scope, op.Name, err)
			return 1
		}
	}

	// Apply-time lint for unconditional loop shapes (self-loops and
	// 2-stack ping-pongs). Warnings only — design-time complement to the
	// runtime budget guards in chassis/processor/budget.go. See
	// chassis/cli/loop_lint.go for the detection logic.
	for _, w := range lintStackLoops(ops) {
		fmt.Fprintf(stderr, "apply: %s\n", w)
	}

	// Mock policy: when the target denies mocks, drop mock_res only.
	// mock_req is documentation/test-fixture metadata, never consulted by
	// the chassis runtime, so it's harmless to preserve.
	if resolved.Mock == "deny" {
		for i := range ops {
			ops[i].MockRes = ""
		}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "validated %d resonator(s) for target %q (chassis: %s, mock: %s):\n",
			len(ops), resolved.Name, resolved.Chassis, mockOrDefault(resolved.Mock))
		for _, op := range ops {
			fmt.Fprintf(stdout, "  %s/%d/%s\n", op.Stack, op.Scope, op.Name)
		}
		fmt.Fprintln(stdout, "(dry-run; nothing pushed)")
		return 0
	}

	clientTarget := resolveTarget(dir, *target, *addr, *user, *pass, *profile)
	clientTarget.Tenant = resolveTenant(*tenant, *profile)
	c := client.New(clientTarget)

	// Group ops by stack, then for each stack: create a draft (cloning
	// the active version), upload the file set, and activate. `apply`
	// is push+activate sugar — keeps the one-verb dev path while the
	// underlying flow goes through the versioned control plane.
	stacks := groupOpsByStack(ops)
	ctx := context.Background()

	// Upload built compute artifacts before activating any resonator that
	// references them — activation verifies their presence and rolls back if
	// missing.
	if err := uploadComputes(ctx, c, builtComputes, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "apply: %v\n", err)
		return 1
	}

	for _, stack := range sortedKeys(stacks) {
		files := opsToFiles(stacks[stack])
		versionNumber, err := c.CreateDraft(ctx, stack, "active")
		if err != nil {
			// Name the EFFECTIVE endpoint actually dialed — i.e.
			// clientTarget.Addr after --addr/env/profile override, not
			// the raw txco.yaml target (resolved.Chassis), which is
			// pre-override and misleads when --addr is passed. A 401
			// unknown_key usually means the key isn't enrolled on this
			// endpoint (compare `txco auth whoami`).
			fmt.Fprintf(stderr, "apply: %s: create draft: %v\n  (endpoint %s; txco.yaml target %q)\n",
				stack, err, clientTarget.Addr, resolved.Name)
			return 1
		}
		if _, err := c.PutDraftFiles(ctx, stack, versionNumber, files); err != nil {
			fmt.Fprintf(stderr, "apply: %s: upload files for v%d: %v\n", stack, versionNumber, err)
			return 1
		}
		// Pre-activate validation: run the same checks the chassis
		// would run on activate, but surface them before we flip the
		// pointer. On failure we leave the draft on the chassis so the
		// user can either fix locally and re-apply or activate it
		// manually via the admin UI after investigating.
		if !*noValidate {
			vresp, verr := c.ValidateVersion(ctx, stack, versionNumber)
			if verr != nil {
				fmt.Fprintf(stderr, "apply: %s: validate v%d: %v\n", stack, versionNumber, verr)
				return 1
			}
			if vresp != nil && !vresp.OK {
				fmt.Fprintf(stderr, "apply: %s v%d: validation failed (%d error%s); not activating.\n",
					stack, versionNumber, len(vresp.Errors), pluralS(len(vresp.Errors)))
				for _, e := range vresp.Errors {
					fmt.Fprintf(stderr, "  %s: %s\n", e.Path, e.Err)
				}
				fmt.Fprintf(stderr, "apply: draft v%d left on chassis; fix locally and re-run `txco apply`, or `--no-validate` to push anyway.\n", versionNumber)
				return 1
			}
		}
		act, err := c.Activate(ctx, stack, versionNumber)
		if err != nil {
			fmt.Fprintf(stderr, "apply: %s: activate v%d: %v\n", stack, versionNumber, err)
			return 1
		}
		if act.PriorVersionNumber != nil {
			fmt.Fprintf(stdout, "%s v%d activated (was v%d, %d files)\n",
				stack, act.VersionNumber, *act.PriorVersionNumber, len(files))
		} else {
			fmt.Fprintf(stdout, "%s v%d activated (%d files)\n",
				stack, act.VersionNumber, len(files))
		}
		if act.StructuredURL != "" {
			fmt.Fprintf(stdout, "  → %s\n", act.StructuredURL)
		}
	}
	return 0
}

// groupOpsByStack collects parsed bundle entries per top-level stack
// name. Nested stacks (e.g. "website/canary") stay distinct from
// their parents — each is its own versioning unit.
func groupOpsByStack(ops []bundle.Op) map[string][]bundle.Op {
	out := map[string][]bundle.Op{}
	for _, op := range ops {
		out[op.Stack] = append(out[op.Stack], op)
	}
	return out
}

func sortedKeys(m map[string][]bundle.Op) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// opsToFiles converts parsed bundle entries (one per resonator) into
// stack_files entries the server stores. Layout mirrors the local on-disk
// shape:
//
//	<scope>/<name>.txcl              — resonator body
//	<scope>/mock-request.json        — shared mock_req for the scope
//	<scope>/mock-response.json       — shared mock_res for the scope
//
// The mock files attach to all resonators at that scope, so we emit one copy
// per scope rather than per name; the first non-empty value wins (the
// bundle walker already enforces consistency).
func opsToFiles(ops []bundle.Op) []client.StackFile {
	var out []client.StackFile
	seenMockReq := map[int]bool{}
	seenMockRes := map[int]bool{}
	for _, op := range ops {
		out = append(out, client.StackFile{
			Path:    strconv.Itoa(op.Scope) + "/" + op.Name + ".txcl",
			Content: op.Txcl,
		})
		if op.MockReq != "" && !seenMockReq[op.Scope] {
			out = append(out, client.StackFile{
				Path:    strconv.Itoa(op.Scope) + "/mock-request.json",
				Content: op.MockReq,
			})
			seenMockReq[op.Scope] = true
		}
		if op.MockRes != "" && !seenMockRes[op.Scope] {
			out = append(out, client.StackFile{
				Path:    strconv.Itoa(op.Scope) + "/mock-response.json",
				Content: op.MockRes,
			})
			seenMockRes[op.Scope] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// localManifestHash mirrors the chassis-side hash at
// chassis/server/admin/stacks.go:computeManifestHash: sha256 over the
// sorted (path NUL content_hash NUL) pairs, where content_hash is
// sha256(content). Stable for a given file set regardless of input
// order. Lets the CLI ask "do we even need to push?" without uploading
// — when local matches the chassis's active manifest, the dev loop
// can skip create-draft/push/activate entirely.
func localManifestHash(files []client.StackFile) string {
	type pair struct{ path, hash string }
	pairs := make([]pair, 0, len(files))
	for _, f := range files {
		ch := sha256.Sum256([]byte(f.Content))
		pairs = append(pairs, pair{f.Path, hex.EncodeToString(ch[:])})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].path < pairs[j].path })
	h := sha256.New()
	for _, p := range pairs {
		h.Write([]byte(p.path))
		h.Write([]byte{0})
		h.Write([]byte(p.hash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildOpRefMap converts the resolved target's operationConfig map into the
// shape the oprefs package consumes.
func buildOpRefMap(t ResolvedTarget) map[string]oprefs.Operation {
	out := make(map[string]oprefs.Operation, len(t.Operations))
	for name, op := range t.Operations {
		out[name] = oprefs.Operation{URL: op.URL}
	}
	return out
}

// resolveOpRefsColocated substitutes each resonator's op://NAME references. A
// colocated sibling <resonatordir>/NAME.js (or NAME.ts) — a local compute —
// takes precedence: it's built (cached under workspaceRoot/.txco/compute) and the
// ref becomes compute://sha256/<digest>; otherwise the global urlMap (txco.yaml
// operations URLs, for remote workers) is used. Resolution is PER-RESONATOR — two
// scopes may each have a different NAME.js — so the colocated entries never leak
// across resonators. Returns the substituted ops + the built computes (deduped by
// digest) to upload.
func resolveOpRefsColocated(ops []bundle.Op, urlMap map[string]oprefs.Operation, workspaceRoot string, stderr io.Writer) ([]bundle.Op, []computeapi.Built, error) {
	out := make([]bundle.Op, len(ops))
	var built []computeapi.Built
	seen := map[string]bool{}
	for i, op := range ops {
		out[i] = op
		if !oprefs.HasRefs(op.Txcl) {
			continue
		}
		// op.SourcePath is relative to the workspace root (Walk uses
		// os.DirFS(root)), so join it back to find siblings on disk.
		resonatorDir := filepath.Join(workspaceRoot, filepath.Dir(op.SourcePath))
		perResonator := urlMap // shared until a colocated ref forces a copy
		copied := false
		for _, name := range oprefs.References(op.Txcl) {
			// Colocated sibling, preferring .js then .ts.
			src := ""
			for _, ext := range []string{".js", ".ts"} {
				p := filepath.Join(resonatorDir, name+ext)
				if _, e := os.Stat(p); e == nil {
					src = p
					break
				}
			}
			if src == "" {
				continue // not colocated → fall through to the URL map
			}
			b, berr := computeapi.BuildFile(src, workspaceRoot)
			if berr != nil {
				return nil, nil, fmt.Errorf("op://%s: %v", name, berr)
			}
			if !copied {
				perResonator = cloneOpRefMap(urlMap)
				copied = true
			}
			perResonator[name] = oprefs.Operation{URL: b.Ref}
			if !seen[b.Digest] {
				seen[b.Digest] = true
				built = append(built, b)
			}
		}
		sub, err := oprefs.ResolveOpRefs(op.Txcl, perResonator)
		if err != nil {
			return nil, nil, fmt.Errorf("%s (%s/%d/%s): %v", op.SourcePath, op.Stack, op.Scope, op.Name, err)
		}
		out[i].Txcl = sub
	}
	return out, built, nil
}

func cloneOpRefMap(m map[string]oprefs.Operation) map[string]oprefs.Operation {
	out := make(map[string]oprefs.Operation, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// uploadComputes HEADs then PUTs each built artifact to the chassis so it is
// present before any referencing resonator is activated (activation verifies
// presence). Idempotent: unchanged modules are skipped. Shared by `apply` and
// `dev`.
func uploadComputes(ctx context.Context, c *client.Client, built []computeapi.Built, stdout, stderr io.Writer) error {
	for _, b := range built {
		if present, herr := c.HeadCompute(ctx, b.Alg, b.Digest); herr == nil && present {
			continue
		}
		if err := c.PutCompute(ctx, b.Alg, b.Digest, b.Engine, b.Wasm); err != nil {
			return fmt.Errorf("upload compute %s: %w", b.Ref, err)
		}
		fmt.Fprintf(stdout, "uploaded %s (%d bytes)\n", b.Ref, len(b.Wasm))
	}
	return nil
}

func mockOrDefault(s string) string {
	if s == "" {
		return "allow"
	}
	return s
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
