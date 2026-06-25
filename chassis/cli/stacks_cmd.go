package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

// runPull: `txco pull <stack> [--version N] [--force] [<dir>]`
//
// Materialises the active (or specified) version's files under
// <dir>/OPS/<stack>/... and writes <dir>/.txco/<stack>.state.json so
// subsequent pushes know which version to parent off.
// pullResult is the JSON form of a `txco pull --json`.
type pullResult struct {
	Stack        string `json:"stack"`
	Version      int64  `json:"version"`
	FilesWritten int    `json:"files_written"`
	Dir          string `json:"dir"`
}

// activateResult is the JSON form of a `txco activate --json`.
type activateResult struct {
	Stack        string `json:"stack"`
	Version      int64  `json:"version"`
	PriorVersion *int64 `json:"prior_version,omitempty"`
}

// runCat: `txco cat [flags] <stack> <path>` — resolve a deployed stack's FILES/
// asset (active version) the same way txco://read-file does, and print it.
// Reports where resolution succeeds/fails — an ops + debugging probe.
func runCat(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("cat", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	asJSON := fs.Bool("json", false, "emit the raw diagnostic JSON instead of file content")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco cat [flags] <stack> <path>

Resolve a FILES/ asset of a stack's ACTIVE version exactly the way
txco://read-file does (manifest row -> content-addressed store) and print it to
stdout. Reports on stderr where resolution succeeds or fails (manifest miss /
empty hash / CAS miss). <path> is FILES-relative, e.g.
_data/posts/welcome/index.json

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "cat: usage: txco cat <stack> <path>")
		return 2
	}
	stack, filePath := fs.Arg(0), fs.Arg(1)

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "cat: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	fc, err := c.CatFile(context.Background(), stack, filePath)
	if err != nil {
		fmt.Fprintf(stderr, "cat: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(fc)
		if !fc.Found {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "stack=%s path=%s active_version_id=%d found=%v source=%s hash=%s inline_len=%d\n",
		stack, fc.Path, fc.ActiveVersionID, fc.Found, fc.Source, fc.ContentHash, fc.InlineLen)
	if !fc.Found {
		fmt.Fprintf(stderr, "  ✗ %s\n", fc.Reason)
		return 1
	}
	b, derr := base64.StdEncoding.DecodeString(fc.ContentB64)
	if derr != nil {
		fmt.Fprintf(stderr, "cat: decode content: %v\n", derr)
		return 1
	}
	_, _ = stdout.Write(b)
	return 0
}

func runPull(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("pull", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	versionFlag := fs.Int64("version", 0, "version_number to pull (default: active)")
	force := fs.Bool("force", false, "overwrite local files even if a dirty workspace is detected")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco pull [flags] <stack> [<dir>]

Pull a stack's active (or specified) version into the local workspace.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "pull: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)
	dir, err := workspaceDir(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "pull: resolve dir: %v\n", err)
		return 1
	}

	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	ctx := context.Background()
	versionNumber := *versionFlag
	if versionNumber == 0 {
		st, err := c.GetStack(ctx, stack)
		if err != nil {
			fmt.Fprintf(stderr, "pull: lookup stack %q: %v\n", stack, err)
			return 1
		}
		if st.ActiveVersion == nil {
			fmt.Fprintf(stderr, "pull: stack %q has no active version; specify --version N\n", stack)
			return 1
		}
		versionNumber = *st.ActiveVersion
	}

	vd, err := c.GetVersion(ctx, stack, versionNumber, true)
	if err != nil {
		fmt.Fprintf(stderr, "pull: get version %d: %v\n", versionNumber, err)
		return 1
	}

	stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
	if !*force {
		// Manifest-aware dirty check: if local content matches the
		// state file's recorded manifest_hash, the workspace is clean
		// relative to the last pull (or push) and a pull is safe.
		// When state is missing or local diverges, fall back to the
		// "any local file is dirty" behavior — protects in-flight
		// edits the user hasn't committed back to the chassis yet.
		saved, _ := state.Load(dir, stack)
		clean, err := localStackClean(dir, stack, saved)
		if err != nil {
			fmt.Fprintf(stderr, "pull: check workspace %s: %v\n", stackDir, err)
			return 1
		}
		if !clean {
			if saved != nil && saved.ManifestHash != "" {
				fmt.Fprintf(stderr,
					"pull: workspace %s has uncommitted changes since v%d pull (manifest mismatch); rerun with --force or run `txco diff` to inspect\n",
					stackDir, saved.VersionNumber)
			} else if dirty, why := stackDirty(stackDir); dirty {
				fmt.Fprintf(stderr,
					"pull: workspace %s has %s and no prior pull recorded; rerun with --force to overwrite\n",
					stackDir, why)
			} else {
				fmt.Fprintf(stderr,
					"pull: workspace %s flagged dirty unexpectedly; rerun with --force\n",
					stackDir)
			}
			return 1
		}
	}
	// Wipe existing on-disk files for this stack and re-materialise.
	if err := os.RemoveAll(stackDir); err != nil {
		fmt.Fprintf(stderr, "pull: clear %s: %v\n", stackDir, err)
		return 1
	}
	for _, f := range vd.Files {
		full := filepath.Join(stackDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			fmt.Fprintf(stderr, "pull: mkdir %s: %v\n", filepath.Dir(full), err)
			return 1
		}
		// Binary assets arrive base64-encoded (paired with the server's encode on the
		// version-detail wire); decode to the real bytes before writing.
		data := []byte(f.Content)
		if f.Encoding == "base64" {
			decoded, derr := base64.StdEncoding.DecodeString(f.Content)
			if derr != nil {
				fmt.Fprintf(stderr, "pull: decode %s: %v\n", full, derr)
				return 1
			}
			data = decoded
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "pull: write %s: %v\n", full, err)
			return 1
		}
	}
	if err := state.Save(dir, stack, state.State{
		VersionNumber:       versionNumber,
		ParentVersionNumber: versionNumber,
		ManifestHash:        vd.ManifestHash,
	}); err != nil {
		fmt.Fprintf(stderr, "pull: save state: %v\n", err)
		return 1
	}

	if *asJSON {
		if err := writeJSON(stdout, pullResult{
			Stack: stack, Version: versionNumber, FilesWritten: len(vd.Files), Dir: stackDir,
		}); err != nil {
			fmt.Fprintf(stderr, "pull: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "pulled %s v%d → %s (%d files)\n", stack, versionNumber, stackDir, len(vd.Files))
	return 0
}

// stackDirty reports whether the local stack directory has files not
// recorded in state. v1 heuristic: any file under stackDir without a
// fresh state.json is considered dirty. Returns ("there are files", true)
// or ("", false).
func stackDirty(stackDir string) (bool, string) {
	if _, err := os.Stat(stackDir); err != nil {
		return false, ""
	}
	count := 0
	_ = filepath.WalkDir(stackDir, func(_ string, _ os.DirEntry, _ error) error {
		count++
		return nil
	})
	if count > 1 { // 1 = the stackDir entry itself
		return true, fmt.Sprintf("%d existing entries", count-1)
	}
	return false, ""
}

// localStackClean reports whether the local stack directory's content
// matches the saved state's manifest_hash. Empty dirs and missing state
// both count as "clean" — the caller decides whether to refuse based
// on whether the user has indicated a fresh-start with --force.
//
// Pre-condition rules:
//   - stackDir missing → clean (nothing to overwrite).
//   - saved == nil OR saved.ManifestHash == "" → fall back to stackDirty:
//     if any files exist locally we can't prove they match the chassis,
//     so refuse.
//   - saved set → compute local manifest_hash; clean iff hashes match.
func localStackClean(dir, name string, saved *state.State) (bool, error) {
	stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(name))
	if _, err := os.Stat(stackDir); os.IsNotExist(err) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if saved == nil || saved.ManifestHash == "" {
		// No baseline → can't compute relative cleanliness.
		dirty, _ := stackDirty(stackDir)
		return !dirty, nil
	}
	files, err := loadLocalStackFiles(dir, name)
	if err != nil {
		return false, err
	}
	return localManifestHash(files) == saved.ManifestHash, nil
}

// loadLocalStackFiles reconstructs one stack's file set EXACTLY as `apply` /
// `push` upload it, so a cleanliness hash matches the manifest the push
// recorded. That manifest is `localManifestHash(opsToFiles + collectFileAssets)`:
//   - opsToFiles keys ops by the WALKER's normalized form — NUMERIC scope +
//     FLATTENED name (e.g. "100/slug.txcl", "100/sub_op.txcl") — not the raw
//     on-disk path ("0100_LABEL/slug.txcl", "0100_LABEL/sub/op.txcl").
//   - collectFileAssets adds the stack's own FILES/** ("FILES/<rel>").
//
// An earlier disk-walk implementation read raw paths, so it matched only stacks
// using plain-numeric single-level scope dirs. Every labeled-scope-dir
// (`<NNNN>_LABEL/`), nested-op, or FILES-bearing stack falsely read
// "edited since pull" even right after a clean push. Walking with bundle.Walk
// (the same walker the push uses) and routing through opsToFiles makes the two
// bases identical. dir is the workspace root (holds OPS/); name is the stack.
func loadLocalStackFiles(dir, name string) ([]client.StackFile, error) {
	ops, err := bundle.Walk(dir)
	if err != nil {
		return nil, err
	}
	files := opsToFiles(opsForStack(ops, name))
	// The stack's own static assets (FILES/**), on the same basis the push
	// records them. collectFileAssets walks only this stack's top-level FILES/
	// (not a nested sub-stack's _mail/FILES/), matching opsToFiles scoping.
	assets, err := collectFileAssets(filepath.Join(dir, "OPS", filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	return append(files, assets...), nil
}

// opsForStack narrows a full bundle.Walk result to a single stack's ops.
func opsForStack(ops []bundle.Op, name string) []bundle.Op {
	var out []bundle.Op
	for _, op := range ops {
		if op.Stack == name {
			out = append(out, op)
		}
	}
	return out
}

// runDraft: `txco draft <stack> [--activate] [<dir>]`.
//
// Walks <dir>/OPS/<stack>/..., creates a draft (cloning from the
// active version), uploads the file set, and optionally activates.
// Without --activate the draft is held for review; `txco push <stack>`
// is the create+activate shortcut (and resolves op:// refs like `apply`).
func runDraft(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("draft", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	activate := fs.Bool("activate", false, "activate the new draft immediately (create + activate, like push minus op:// resolution)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco draft [flags] <stack> [<dir>]

Create a draft version of <stack> from local OPS/<stack>/... and upload
its files. Without --activate the draft is held back for review.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "draft: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)
	dir, err := workspaceDir(fs.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "draft: resolve dir: %v\n", err)
		return 1
	}

	stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
	files, err := collectStackFiles(stackDir)
	if err != nil {
		fmt.Fprintf(stderr, "draft: walk %s: %v\n", stackDir, err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintf(stderr, "draft: no files under %s\n", stackDir)
		return 1
	}

	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	ctx := context.Background()
	versionNumber, err := c.CreateDraft(ctx, stack, "active")
	if err != nil {
		fmt.Fprintf(stderr, "draft: create draft: %v\n", err)
		return 1
	}
	if _, err := c.PutDraftFiles(ctx, stack, versionNumber, files); err != nil {
		fmt.Fprintf(stderr, "draft: upload files for v%d: %v\n", versionNumber, err)
		return 1
	}
	res := deployResult{Stack: stack, Version: versionNumber, Files: len(files)}
	if !*asJSON {
		fmt.Fprintf(stdout, "drafted %s v%d (%d files)\n", stack, versionNumber, len(files))
	}

	if *activate {
		act, err := c.Activate(ctx, stack, versionNumber)
		if err != nil {
			fmt.Fprintf(stderr, "draft: activate v%d: %v\n", versionNumber, err)
			return 1
		}
		res.Activated = true
		res.PriorVersion = act.PriorVersionNumber
		if !*asJSON {
			if act.PriorVersionNumber != nil {
				fmt.Fprintf(stdout, "activated %s v%d (was v%d)\n", stack, act.VersionNumber, *act.PriorVersionNumber)
			} else {
				fmt.Fprintf(stdout, "activated %s v%d\n", stack, act.VersionNumber)
			}
		}
	}
	if *asJSON {
		if err := writeJSON(stdout, res); err != nil {
			fmt.Fprintf(stderr, "draft: encode json: %v\n", err)
			return 1
		}
	}
	return 0
}

// collectStackFiles walks stackDir and returns one StackFile per file.
// Path is stored relative to stackDir, slash-separated regardless of
// host OS. Symlinks and dotfiles are skipped.
func collectStackFiles(stackDir string) ([]client.StackFile, error) {
	var out []client.StackFile
	err := filepath.WalkDir(stackDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(filepath.Base(p), ".") && p != stackDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), ".") {
			return nil
		}
		rel, err := filepath.Rel(stackDir, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sf := client.StackFile{Path: filepath.ToSlash(rel), Content: string(content)}
		// Binary (non-UTF-8) assets → base64 for the JSON wire (the server decodes);
		// otherwise JSON's invalid-UTF-8 → U+FFFD rewrite corrupts them.
		if !utf8.Valid(content) {
			sf.Content = base64.StdEncoding.EncodeToString(content)
			sf.Encoding = "base64"
		}
		out = append(out, sf)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// runActivate: `txco activate <stack> [--version N]`
//
// Without --version, picks the most recent draft (or refuses if none).
// With --version, activates that exact version_number. Flags may appear
// before or after <stack> (pflag parses positionals and flags in any order).
func runActivate(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("activate", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	versionFlag := fs.Int64("version", 0, "version_number to activate (default: most recent draft)")
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco activate [flags] <stack>

Flip the active version of <stack>. Without --version, activates the
most recent draft (errors if none exists).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "activate: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "activate: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	ctx := context.Background()
	versionNumber := *versionFlag
	if versionNumber == 0 {
		versions, err := c.ListVersions(ctx, stack)
		if err != nil {
			fmt.Fprintf(stderr, "activate: list versions: %v\n", err)
			return 1
		}
		for _, v := range versions {
			if v.Status == "draft" {
				versionNumber = v.VersionNumber
				break
			}
		}
		if versionNumber == 0 {
			fmt.Fprintf(stderr, "activate: no draft to activate; pass --version N\n")
			return 1
		}
	}

	act, err := c.Activate(ctx, stack, versionNumber)
	if err != nil {
		fmt.Fprintf(stderr, "activate: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := writeJSON(stdout, activateResult{
			Stack: stack, Version: act.VersionNumber, PriorVersion: act.PriorVersionNumber,
		}); err != nil {
			fmt.Fprintf(stderr, "activate: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if act.PriorVersionNumber != nil {
		fmt.Fprintf(stdout, "activated %s v%d (was v%d)\n", stack, act.VersionNumber, *act.PriorVersionNumber)
	} else {
		fmt.Fprintf(stdout, "activated %s v%d\n", stack, act.VersionNumber)
	}
	return 0
}

// runDeactivate: `txco deactivate <stack>` — the inverse of `txco activate`.
//
// Retires a stack by activating an EMPTY version: the stack stops serving
// (HTTP 404 / mail 550) but its version history is kept, so a later
// `txco apply`/`txco activate` brings it back. Use this when you've removed
// a stack from your local OPS/ tree — `apply` only re-versions the stacks it
// still finds, so a deleted stack keeps serving its last active version until
// you deactivate it here. Fleet-safe: it propagates via the normal activation
// path, so every node converges.
func runDeactivate(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("deactivate", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of human output")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco deactivate [flags] <stack>

Retire <stack> by activating an empty version: it stops serving (HTTP 404 /
mail 550), but its version history is kept so `+"`txco apply`"+` restores it.
Use after deleting a stack locally — `+"`apply`"+` won't remove a stack it no
longer finds.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "deactivate: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "deactivate: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	act, err := c.DeactivateStack(context.Background(), stack)
	if err != nil {
		fmt.Fprintf(stderr, "deactivate: %v\n", err)
		return 1
	}
	if *asJSON {
		if err := writeJSON(stdout, activateResult{
			Stack: stack, Version: act.VersionNumber, PriorVersion: act.PriorVersionNumber,
		}); err != nil {
			fmt.Fprintf(stderr, "deactivate: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	if act.PriorVersionNumber != nil {
		fmt.Fprintf(stdout, "deactivated %s — activated empty v%d (was v%d); stack no longer serves\n",
			stack, act.VersionNumber, *act.PriorVersionNumber)
	} else {
		fmt.Fprintf(stdout, "deactivated %s — activated empty v%d; stack no longer serves\n",
			stack, act.VersionNumber)
	}
	return 0
}

// runVersions: `txco versions <stack>` — list versions reverse chronologically.
func runVersions(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("versions", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON instead of the table")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco versions [flags] <stack>

List versions for a stack, newest first. `+"`★`"+` marks the active version.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "versions: missing <stack> argument")
		return 2
	}
	stack := fs.Arg(0)

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "versions: resolve dir: %v\n", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, tf.Profile)
	c := client.New(clientTarget)

	versions, err := c.ListVersions(context.Background(), stack)
	if err != nil {
		fmt.Fprintf(stderr, "versions: %v\n", err)
		return 1
	}
	if *asJSON {
		if versions == nil {
			versions = []client.VersionRecord{}
		}
		if err := writeJSON(stdout, versions); err != nil {
			fmt.Fprintf(stderr, "versions: encode json: %v\n", err)
			return 1
		}
		return 0
	}
	for _, v := range versions {
		marker := "  "
		if v.IsActive {
			marker = "★ "
		}
		fmt.Fprintf(stdout, "%sv%-4d %-12s %s by %s\n", marker, v.VersionNumber, v.Status, v.CreatedAt, v.CreatedBy)
	}
	return 0
}

// quietWalkBundle exists so `txco diff` and `txco push` can share the
// "walk OPS/" parser without duplicating the bundle.Walk import-path
// dance. Returns the parsed Op list or an error.
//
// (Currently unused — push reads files directly so the chassis can
// drive the path-to-(scope,name) mapping. Kept here because it'll be
// needed when push grows op:// substitution.)
//
//nolint:unused
func quietWalkBundle(dir string) ([]bundle.Op, error) {
	return bundle.Walk(dir)
}
