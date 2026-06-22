package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	computeapi "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// applyOpts carries the resolved flag values the deploy pipeline needs. It's
// shared by `apply` (whole workspace) and `push` (one stack) so both deploy
// through the identical path — see applyOps.
type applyOpts struct {
	*targetFlags
	dryRun, noValidate, jsonOut bool
	timeout                     time.Duration
}

// spin animates a braille spinner on a TTY while fn runs, then clears the line.
// On a non-terminal writer (pipe, CI, log) it just runs fn — no control bytes.
func spin(w io.Writer, msg string, fn func() error) error {
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return fn()
	}
	stop := make(chan struct{})
	go func() {
		frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		tk := time.NewTicker(90 * time.Millisecond)
		defer tk.Stop()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-tk.C:
				fmt.Fprintf(w, "\r\x1b[2K%c %s", frames[i%len(frames)], msg)
			}
		}
	}()
	err := fn()
	close(stop)
	fmt.Fprint(w, "\r\x1b[2K")
	return err
}

// deployResult is the per-stack JSON form of a deploy, shared by the --json
// paths of `apply`, `push`, and `draft`. PriorVersion is present only when a
// prior active version was replaced; Activated is false for a `draft` held
// back from activation.
type deployResult struct {
	Stack        string `json:"stack"`
	Version      int64  `json:"version"`
	PriorVersion *int64 `json:"prior_version,omitempty"`
	Files        int    `json:"files"`
	Activated    bool   `json:"activated"`
}

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
	fs := pflag.NewFlagSet("apply", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := applyOpts{targetFlags: bindTargetFlags(fs)}
	fs.BoolVar(&opts.dryRun, "dry-run", false, "validate the bundle locally; do not POST")
	fs.BoolVar(&opts.noValidate, "no-validate", false, "skip server-side validation before activate (push+activate even if the chassis flags errors)")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable JSON (array of per-stack deploy results)")
	fs.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "per-request timeout for chassis calls; raise for large FILE uploads (e.g. 10m)")
	verbose := fs.Bool("verbose", false, "trace every HTTP request/response (method, URL, status, error body) to stderr. Equivalent to TXCO_VERBOSE=1, which works for ANY txco command.")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco apply [flags] [<dir>]

Walk <dir>/OPS/ for *.txcl resonator files, resolve any "op://NAME" references
using the selected target's operations map, validate each resonator, and push
the bundle to that target's chassis admin endpoint. Deploys (creates + activates
a version for) every stack in the tree — use `+"`txco push <stack>`"+` for one stack.

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

	// workspaceDir resolves <dir> (or cwd) then walks up to the OPS/ root so
	// `txco apply` works from anywhere in the tree.
	dir, err := workspaceDir(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "apply: resolve dir: %v\n", err)
		return 1
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
	return applyOps("apply", dir, ops, opts, "", stdout, stderr)
}

// runPush deploys a SINGLE named stack — the inverse of `txco pull <stack>`.
// It runs the exact same pipeline as `apply` (resolve op:// refs, build
// colocated computes, validate, activate) but scoped to one stack, so
// `txco push api` deploys byte-identically to what `txco apply` would do for
// the api stack. `txco draft <stack>` stages without activating.
//
//	txco push <stack> [<dir>]
func runPush(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("push", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := applyOpts{targetFlags: bindTargetFlags(fs)}
	fs.BoolVar(&opts.dryRun, "dry-run", false, "validate locally; do not push")
	fs.BoolVar(&opts.noValidate, "no-validate", false, "skip server-side validation before activate")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit machine-readable JSON (the deploy result object)")
	fs.DurationVar(&opts.timeout, "timeout", 5*time.Minute, "per-request timeout for chassis calls; raise for large FILE uploads (e.g. 10m)")
	verbose := fs.Bool("verbose", false, "trace every HTTP request/response to stderr (TXCO_VERBOSE=1)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco push <stack> [<dir>]

Deploy a single stack — the inverse of `+"`txco pull <stack>`"+`. Builds
OPS/<stack>/ (resolving op:// refs + colocated computes), validates, and
activates it on the target chassis in one step.

Use `+"`txco apply`"+` to deploy the whole workspace, or `+"`txco draft <stack>`"+`
to stage a version for review without activating.

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
	if fs.NArg() < 1 {
		fmt.Fprint(stderr, "push: missing <stack> argument\n\nUsage: txco push <stack> [<dir>]\n")
		return 2
	}
	stack := fs.Arg(0)

	dir, err := workspaceDir(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "push: resolve dir: %v\n", err)
		return 1
	}

	ops, err := bundle.Walk(dir)
	if err != nil {
		fmt.Fprintf(stderr, "push: walk %s: %v\n", dir, err)
		return 1
	}
	if len(ops) == 0 {
		fmt.Fprintf(stderr, "push: no resonators found — expected an OPS/ tree at or above %s.\n", dir)
		return 1
	}
	return applyOps("push", dir, ops, opts, stack, stdout, stderr)
}

// applyOps is the shared deploy pipeline behind `apply` and `push`. cmd names
// the calling verb (for error prefixes). When onlyStack is non-empty, ops are
// filtered to that one stack first (the `push` path); "" deploys all stacks
// (the `apply` path). The flow: resolve op:// refs (+ build colocated
// computes), client-side parse, loop-lint, apply the target's mock policy,
// then per stack create a draft, upload, validate, and activate.
func applyOps(cmd, dir string, ops []bundle.Op, opts applyOpts, onlyStack string, stdout, stderr io.Writer) int {
	if onlyStack != "" {
		filtered := make([]bundle.Op, 0, len(ops))
		for _, op := range ops {
			if op.Stack == onlyStack {
				filtered = append(filtered, op)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(stderr, "%s: stack %q not found under %s/OPS/", cmd, onlyStack, dir)
			if avail := sortedKeys(groupOpsByStack(ops)); len(avail) > 0 {
				fmt.Fprintf(stderr, " (available: %s)", strings.Join(avail, ", "))
			}
			fmt.Fprintln(stderr)
			return 1
		}
		ops = filtered
	}

	// Resolve the selected target so we know which operations map to use
	// and whether mock_res should be stripped.
	resolved := resolveFullTarget(dir, opts.Target)
	urlMap := buildOpRefMap(resolved)

	// Substitute op://NAME per resonator: a colocated <resonatordir>/NAME.js compute wins
	// (built + uploaded), else the txco.yaml operations URL. Computes build
	// into .txco/compute and are uploaded after the client is constructed
	// (skipped on --dry-run).
	subOps, builtComputes, cerr := resolveOpRefsColocated(ops, urlMap, dir, stderr)
	if cerr != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, cerr)
		return 1
	}
	ops = subOps
	if len(builtComputes) > 0 {
		_, _ = ensureGitignored(dir, ".txco/")
	}

	// Client-side parse — fail fast before contacting the server.
	for _, op := range ops {
		if _, err := txcl.Resonator(op.Txcl); err != nil {
			fmt.Fprintf(stderr, "%s: parse error at %s (%s/%d/%s): %v\n",
				cmd, op.SourcePath, op.Stack, op.Scope, op.Name, err)
			return 1
		}
	}

	// Apply-time lint for unconditional loop shapes (self-loops and
	// 2-stack ping-pongs). Warnings only — design-time complement to the
	// runtime budget guards in chassis/processor/budget.go. See
	// chassis/cli/loop_lint.go for the detection logic.
	for _, w := range lintStackLoops(ops) {
		fmt.Fprintf(stderr, "%s: %s\n", cmd, w)
	}

	// Mock policy: when the target denies mocks, drop mock_res only.
	// mock_req is documentation/test-fixture metadata, never consulted by
	// the chassis runtime, so it's harmless to preserve.
	if resolved.Mock == "deny" {
		for i := range ops {
			ops[i].MockRes = ""
		}
	}

	if opts.dryRun {
		if opts.jsonOut {
			type resonator struct {
				Stack string `json:"stack"`
				Scope int    `json:"scope"`
				Name  string `json:"name"`
			}
			rs := make([]resonator, 0, len(ops))
			for _, op := range ops {
				rs = append(rs, resonator{op.Stack, op.Scope, op.Name})
			}
			if err := writeJSON(stdout, map[string]any{
				"dry_run": true, "target": resolved.Name, "resonators": rs,
			}); err != nil {
				fmt.Fprintf(stderr, "%s: encode json: %v\n", cmd, err)
				return 1
			}
			return 0
		}
		fmt.Fprintf(stdout, "validated %d resonator(s) for target %q (chassis: %s, mock: %s):\n",
			len(ops), resolved.Name, resolved.Chassis, mockOrDefault(resolved.Mock))
		for _, op := range ops {
			fmt.Fprintf(stdout, "  %s/%d/%s\n", op.Stack, op.Scope, op.Name)
		}
		fmt.Fprintln(stdout, "(dry-run; nothing pushed)")
		return 0
	}

	clientTarget := resolveTarget(dir, opts.Target, opts.Addr, opts.User, opts.Pass, opts.Profile)
	clientTarget.Tenant = resolveTenant(opts.Tenant, opts.Profile)
	c := client.NewWithTimeout(clientTarget, opts.timeout)
	ctx := context.Background()

	// In --json mode stdout must carry only the result object, so route
	// progress chatter (compute uploads) to stderr.
	progress := stdout
	if opts.jsonOut {
		progress = stderr
	}

	// Upload built compute artifacts before activating any resonator that
	// references them — activation verifies their presence and rolls back if
	// missing.
	if err := uploadComputes(ctx, c, builtComputes, progress, stderr); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return 1
	}

	// Group ops by stack, then for each stack: create a draft (cloning
	// the active version), upload the file set, validate, and activate.
	// `apply`/`push` are push+activate sugar over the versioned control plane.
	stacks := groupOpsByStack(ops)
	results := make([]deployResult, 0, len(stacks))
	for _, stack := range sortedKeys(stacks) {
		files := opsToFiles(stacks[stack])
		assets, aerr := collectFileAssets(filepath.Join(dir, "OPS", stack))
		if aerr != nil {
			fmt.Fprintf(stderr, "%s: %s: collect FILES/: %v\n", cmd, stack, aerr)
			return 1
		}
		files = append(files, assets...)
		versionNumber, err := c.CreateDraft(ctx, stack, "active")
		if err != nil {
			// Name the EFFECTIVE endpoint actually dialed — i.e.
			// clientTarget.Addr after --addr/env/profile override, not
			// the raw txco.yaml target (resolved.Chassis), which is
			// pre-override and misleads when --addr is passed. A 401
			// unknown_key usually means the key isn't enrolled on this
			// endpoint (compare `txco auth whoami`).
			fmt.Fprintf(stderr, "%s: %s: create draft: %v\n  (endpoint %s; txco.yaml target %q)\n",
				cmd, stack, err, clientTarget.Addr, resolved.Name)
			return 1
		}
		if err := spin(progress, fmt.Sprintf("uploading %d files → %s v%d", len(files), stack, versionNumber), func() error {
			_, e := c.PutDraftFiles(ctx, stack, versionNumber, files)
			return e
		}); err != nil {
			fmt.Fprintf(stderr, "%s: %s: upload files for v%d: %v\n", cmd, stack, versionNumber, err)
			return 1
		}
		// Pre-activate validation: run the same checks the chassis
		// would run on activate, but surface them before we flip the
		// pointer. On failure we leave the draft on the chassis so the
		// user can either fix locally and re-deploy or activate it
		// manually via the admin UI after investigating.
		if !opts.noValidate {
			var vresp *client.ValidateResponse
			if err := spin(progress, fmt.Sprintf("validating %s v%d", stack, versionNumber), func() error {
				var e error
				vresp, e = c.ValidateVersion(ctx, stack, versionNumber)
				return e
			}); err != nil {
				fmt.Fprintf(stderr, "%s: %s: validate v%d: %v\n", cmd, stack, versionNumber, err)
				return 1
			}
			if vresp != nil && !vresp.OK {
				fmt.Fprintf(stderr, "%s: %s v%d: validation failed (%d error%s); not activating.\n",
					cmd, stack, versionNumber, len(vresp.Errors), pluralS(len(vresp.Errors)))
				for _, e := range vresp.Errors {
					fmt.Fprintf(stderr, "  %s: %s\n", e.Path, e.Err)
				}
				fmt.Fprintf(stderr, "%s: draft v%d left on chassis; fix locally and re-run, or `--no-validate` to push anyway.\n", cmd, versionNumber)
				return 1
			}
		}
		var act *client.ActivateResponse
		if err := spin(progress, fmt.Sprintf("activating %s v%d", stack, versionNumber), func() error {
			var e error
			act, e = c.Activate(ctx, stack, versionNumber)
			return e
		}); err != nil {
			fmt.Fprintf(stderr, "%s: %s: activate v%d: %v\n", cmd, stack, versionNumber, err)
			return 1
		}
		// Record local state so `txco status` shows this stack in sync. Push
		// deployed the LOCAL files as v<act.VersionNumber>, so the workspace now
		// mirrors that version's content exactly — the same invariant a fresh
		// `pull` establishes. ManifestHash is computed on the same basis status
		// recomputes (localManifestHash), so the stack reads "(clean)" right
		// after. Best-effort: the deploy already succeeded, so a state-write
		// failure only warns — it must not fail the push.
		if serr := state.Save(dir, stack, state.State{
			VersionNumber:       act.VersionNumber,
			ParentVersionNumber: act.VersionNumber,
			ManifestHash:        localManifestHash(files),
		}); serr != nil {
			fmt.Fprintf(stderr, "%s: %s: warning: could not record local state: %v\n", cmd, stack, serr)
		}
		results = append(results, deployResult{
			Stack: stack, Version: act.VersionNumber,
			PriorVersion: act.PriorVersionNumber, Files: len(files), Activated: true,
		})
		if opts.jsonOut {
			continue
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

	if opts.jsonOut {
		// `push` deploys exactly one stack → emit a single object; `apply`
		// emits the array (one entry per stack).
		var payload any = results
		if onlyStack != "" && len(results) == 1 {
			payload = results[0]
		}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "%s: encode json: %v\n", cmd, err)
			return 1
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

// collectFileAssets walks <stackDir>/FILES/** and returns one StackFile per
// asset, path-prefixed "FILES/<rel>" (slash-separated). These are the
// tenant's static assets — served by txco://static / content-addressed in
// the filecas store on activation — which the resonator bundle (opsToFiles)
// does not carry. Callers append the result to opsToFiles output so apply /
// dev publish assets alongside rules. Dotfiles/dotdirs and irregular files
// (symlinks) are skipped (mirrors the static index's "."-prefix exclusion
// and collectStackFiles). An absent FILES/ dir yields nil, no error.
//
// NOTE: content travels as a JSON/TEXT string, so non-UTF-8 binary assets
// can be mangled on this path (the documented dev/inline limitation — the
// fleet S3 path carries raw bytes). Keep workspace FILES/ textual until the
// stack_files column is BLOB.
func collectFileAssets(stackDir string) ([]client.StackFile, error) {
	filesDir := filepath.Join(stackDir, "FILES")
	info, err := os.Stat(filesDir)
	if err != nil || !info.IsDir() {
		return nil, nil // no FILES/ → nothing to collect
	}
	var out []client.StackFile
	walkErr := filepath.WalkDir(filesDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != filesDir && strings.HasPrefix(filepath.Base(p), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(filepath.Base(p), ".") {
			return nil
		}
		rel, rerr := filepath.Rel(stackDir, p) // → "FILES/<...>"
		if rerr != nil {
			return rerr
		}
		content, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		out = append(out, client.StackFile{
			Path:    filepath.ToSlash(rel),
			Content: string(content),
		})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
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
			// A prebuilt <name>.wasm sibling (shipped in the package) wins: no
			// toolchain needed, and the digest is identical for every consumer.
			if wp := filepath.Join(resonatorDir, name+".wasm"); fileExists(wp) {
				wb, rerr := os.ReadFile(wp)
				if rerr != nil {
					return nil, nil, fmt.Errorf("op://%s: read %s: %w", name, wp, rerr)
				}
				b := computeapi.BuiltFromWasm(wb)
				if !copied {
					perResonator = cloneOpRefMap(urlMap)
					copied = true
				}
				perResonator[name] = oprefs.Operation{URL: b.Ref}
				if !seen[b.Digest] {
					seen[b.Digest] = true
					built = append(built, b)
				}
				continue
			}
			// Colocated source sibling, preferring .js then .ts — built at apply.
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

// fileExists reports whether path names an existing file (or dir).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
