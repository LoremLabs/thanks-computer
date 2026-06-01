package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
)

// This file holds the package-lifecycle verbs that operate on the workspace
// lockfile + materialized OPS/ tree: `txco package list|upgrade|remove`. They
// all share the install-time edit-detection guard (stackEditState, pkgshared.go)
// so none silently clobbers hand-edited rules. `install` stays the one
// top-level consumer verb; these are routed under `package` (package_cmd.go).

// shortDigest returns the first 12 hex chars of the sha256 in an
// oci://…@sha256:… reference, or "" when there is no digest (dir:/github:
// installs leave Resolved blank).
func shortDigest(resolved string) string {
	const marker = "sha256:"
	i := strings.Index(resolved, marker)
	if i < 0 {
		return ""
	}
	h := resolved[i+len(marker):]
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

// verDigest renders "<version> <digest12>" for output, or just "<version>" when
// the source has no digest.
func verDigest(version, resolved string) string {
	if d := shortDigest(resolved); d != "" {
		return version + " " + d
	}
	return version
}

// runList prints the packages installed in this workspace (read from the
// lockfile), flagging any stack whose OPS/ files no longer match what install
// materialized.
//
//	txco package list
//	txco package list --json
func runList(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package list", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco package list [--json]

Show the packages installed in this workspace (read from %s), with an "edited?"
column when a stack's OPS/ files no longer match what was installed.
`, lockfile.FileName)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	root := workspaceRootOrCwd()
	lf, err := lockfile.Read(root)
	if err != nil {
		fmt.Fprintf(stderr, "package list: %v\n", err)
		return 1
	}

	type listRow struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		InstalledAs string `json:"installedAs,omitempty"`
		Mode        string `json:"mode"`
		Ref         string `json:"ref"`
		Resolved    string `json:"resolved,omitempty"`
		DigestShort string `json:"digestShort,omitempty"`
		Edited      *bool  `json:"edited"`  // null for vendor-only
		Present     bool   `json:"present"` // false when OPS/<stack>/ is gone
	}

	rows := make([]listRow, 0, len(lf.Packages))
	for _, e := range lf.Packages {
		r := listRow{
			Name: e.Name, Version: e.Version, InstalledAs: e.InstalledAs,
			Mode: e.Mode, Ref: e.Ref, Resolved: e.Resolved,
			DigestShort: shortDigest(e.Resolved), Present: true,
		}
		if e.InstalledAs != "" {
			st, err := stackEditState(root, e)
			if err != nil {
				fmt.Fprintf(stderr, "package list: %s: %v\n", e.InstalledAs, err)
				return 1
			}
			edited := st == "edited"
			r.Edited = &edited
			r.Present = st != "missing"
		}
		rows = append(rows, r)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Fprintf(stderr, "package list: %v\n", err)
			return 1
		}
		return 0
	}

	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no packages installed")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tINSTALLED-AS\tMODE\tDIGEST\tEDITED?")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Name, r.Version, dashIfEmpty(r.InstalledAs), r.Mode,
			dashIfEmpty(r.DigestShort), editedCell(r.Edited, r.Present))
	}
	_ = tw.Flush()
	return 0
}

// runUpgrade re-resolves each installed package's recorded ref and
// re-materializes OPS/<stack>/ when the content changed, updating the lockfile.
// Upgrade re-pulls whatever the ref points to now — a pinned version stays put;
// moving to a different version is `install <newref> --as <stack>`.
//
//	txco package upgrade support
//	txco package upgrade --all --dry-run
func runUpgrade(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package upgrade", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	all := fs.Bool("all", false, "upgrade every installed as-stack package")
	dryRun := fs.Bool("dry-run", false, "show what would change; mutate nothing")
	force := fs.Bool("force", false, "overwrite a stack that has local edits")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco package upgrade <stack>… | --all [--dry-run] [--force]

Re-resolve each installed package's ref and re-materialize OPS/<stack>/ when the
content changed, updating %s. Upgrade re-pulls whatever the recorded ref points
to now; a pinned version stays put. To move to a different version, run
`+"`txco install <newref> --as <stack>`"+`.
`, lockfile.FileName)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *all && fs.NArg() > 0 {
		fmt.Fprintln(stderr, "package upgrade: pass either <stack> names or --all, not both")
		return 2
	}
	if !*all && fs.NArg() == 0 {
		fs.Usage()
		return 2
	}

	root := workspaceRootOrCwd()
	lf, err := lockfile.Read(root)
	if err != nil {
		fmt.Fprintf(stderr, "package upgrade: %v\n", err)
		return 1
	}

	var targets []lockfile.Entry
	if *all {
		for _, e := range lf.Packages {
			if e.Mode == "as-stack" {
				targets = append(targets, e)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintln(stdout, "no as-stack packages installed")
			return 0
		}
	} else {
		for _, name := range fs.Args() {
			stack := strings.Trim(name, "/")
			e := lf.FindStack(stack)
			if e == nil {
				fmt.Fprintf(stderr, "package upgrade: %q is not an installed stack (see `txco package list`)\n", stack)
				return 1
			}
			targets = append(targets, *e)
		}
	}

	reg := workspaceRegistry(root)
	var upgraded, upToDate, failed int
	dirty := false
	for _, e := range targets {
		switch upgradeOne(root, reg, lf, e, *force, *dryRun, &dirty, stdout, stderr) {
		case "upgraded":
			upgraded++
		case "uptodate":
			upToDate++
		default:
			failed++
		}
	}

	if dirty && !*dryRun {
		if err := lockfile.Write(root, lf); err != nil {
			fmt.Fprintf(stderr, "package upgrade: write lockfile: %v\n", err)
			return 1
		}
	}
	if len(targets) > 1 || failed > 0 {
		fmt.Fprintf(stdout, "\n%d upgraded, %d up to date, %d failed\n", upgraded, upToDate, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// upgradeOne handles a single upgrade target. It returns "upgraded",
// "uptodate", or "failed", mutating lf (and setting *dirty) on a real upgrade.
// Errors are printed here so `--all` can continue past a failing target.
func upgradeOne(root string, reg registryConfig, lf *lockfile.File, e lockfile.Entry, force, dryRun bool, dirty *bool, stdout, stderr io.Writer) string {
	// Edit guard — never overwrite hand-edited rules without --force.
	st, err := stackEditState(root, e)
	if err != nil {
		fmt.Fprintf(stderr, "upgrade %s: %v\n", e.InstalledAs, err)
		return "failed"
	}
	if st == "edited" && !force {
		fmt.Fprintf(stderr, "upgrade %s: OPS/%s/ has local edits since install; review with `txco diff` or re-run with --force\n", e.InstalledAs, e.InstalledAs)
		return "failed"
	}

	// Re-resolve + fetch via the same pipeline install uses.
	staging, prov, cleanup, err := fetchPackage(resolvePackageRef(e.Ref, reg))
	if err != nil {
		fmt.Fprintf(stderr, "upgrade %s: %v\n", e.InstalledAs, err)
		return "failed"
	}
	defer cleanup()

	m, err := manifest.ParseFile(filepath.Join(staging, manifest.FileName))
	if err != nil {
		fmt.Fprintf(stderr, "upgrade %s: %v\n", e.InstalledAs, err)
		return "failed"
	}
	if probs := manifest.Validate(m, os.DirFS(staging), "."); len(probs) > 0 {
		fmt.Fprintf(stderr, "upgrade %s: %s has %d problem%s:\n", e.InstalledAs, nameOr(m.Name, "package"), len(probs), pluralS(len(probs)))
		for _, p := range probs {
			fmt.Fprintf(stderr, "  %v\n", p)
		}
		return "failed"
	}

	ops, err := bundle.WalkFS(os.DirFS(staging), ".")
	if err != nil {
		fmt.Fprintf(stderr, "upgrade %s: %v\n", e.InstalledAs, err)
		return "failed"
	}
	exported := exportedStackNames(ops)
	if len(exported) != 1 {
		fmt.Fprintf(stderr, "upgrade %s: package exports %d stacks (%s); upgrade supports single-stack packages only\n",
			e.InstalledAs, len(exported), strings.Join(exported, ", "))
		return "failed"
	}
	exportedStack := exported[0]
	newHash := localManifestHash(opsToFiles(ops))

	if newHash == e.ManifestHash {
		fmt.Fprintf(stdout, "%s: up to date (%s %s)\n", e.InstalledAs, m.Name, m.Version)
		return "uptodate"
	}

	srcStackDir := filepath.Join(staging, "OPS", exportedStack)
	destStackDir := filepath.Join(root, "OPS", e.InstalledAs)

	if dryRun {
		fmt.Fprintf(stdout, "upgrade (dry-run) %s: %s → %s\n", e.InstalledAs,
			verDigest(e.Version, e.Resolved), verDigest(m.Version, prov.Reference))
		rels, _ := listTreeRel(srcStackDir)
		for _, rel := range rels {
			fmt.Fprintf(stdout, "    OPS/%s/%s\n", e.InstalledAs, rel)
		}
		return "upgraded"
	}

	if err := os.RemoveAll(destStackDir); err != nil {
		fmt.Fprintf(stderr, "upgrade %s: %v\n", e.InstalledAs, err)
		return "failed"
	}
	if _, err := copyTree(srcStackDir, destStackDir); err != nil {
		fmt.Fprintf(stderr, "upgrade %s: materialize: %v\n", e.InstalledAs, err)
		return "failed"
	}

	lf.Upsert(lockfile.Entry{
		Ref:           e.Ref,
		Registry:      prov.Registry,
		Namespace:     prov.Namespace,
		Name:          m.Name,
		Version:       m.Version,
		Resolved:      prov.Reference,
		ExportedStack: exportedStack,
		InstalledAs:   e.InstalledAs,
		Mode:          "as-stack",
		ManifestHash:  newHash,
		InstalledAt:   lockfile.Now(),
	})
	*dirty = true
	fmt.Fprintf(stdout, "upgraded %s: %s → %s\n", e.InstalledAs,
		verDigest(e.Version, e.Resolved), verDigest(m.Version, prov.Reference))
	warnBundledComputes(m, staging, stdout)
	return "upgraded"
}

// runRemove deletes a package installed as <stack>: removes OPS/<stack>/ (unless
// --keep-files) and drops its lockfile entry.
//
//	txco package remove support
//	txco package remove support --keep-files
func runRemove(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package remove", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	keepFiles := fs.Bool("keep-files", false, "drop the lockfile entry but leave OPS/<stack>/ in place")
	dryRun := fs.Bool("dry-run", false, "show what would change; mutate nothing")
	force := fs.Bool("force", false, "remove even if the stack has local edits")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprintf(stderr, `
Usage: txco package remove <stack> [--keep-files] [--dry-run] [--force]

Remove a package installed as <stack>: delete OPS/<stack>/ and drop its entry
from %s. Use --keep-files to drop only the lockfile entry.
`, lockfile.FileName)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	stack := strings.Trim(fs.Arg(0), "/")

	root := workspaceRootOrCwd()
	lf, err := lockfile.Read(root)
	if err != nil {
		fmt.Fprintf(stderr, "package remove: %v\n", err)
		return 1
	}
	e := lf.FindStack(stack)
	if e == nil {
		fmt.Fprintf(stderr, "package remove: %q is not an installed stack (see `txco package list`)\n", stack)
		return 1
	}

	if !*keepFiles {
		st, err := stackEditState(root, *e)
		if err != nil {
			fmt.Fprintf(stderr, "package remove: %v\n", err)
			return 1
		}
		if st == "edited" && !*force {
			fmt.Fprintf(stderr, "package remove: OPS/%s/ has local edits since install; review with `txco diff`, "+
				"or re-run with --force (discard them) or --keep-files (keep the files)\n", stack)
			return 1
		}
	}

	if *dryRun {
		fmt.Fprintf(stdout, "package remove (dry-run): %s %s installed as %q\n", e.Name, e.Version, stack)
		if *keepFiles {
			fmt.Fprintf(stdout, "  would drop the lockfile entry; OPS/%s/ left in place\n", stack)
		} else {
			fmt.Fprintf(stdout, "  would delete OPS/%s/ and drop the lockfile entry\n", stack)
		}
		return 0
	}

	if !*keepFiles {
		if err := os.RemoveAll(filepath.Join(root, "OPS", stack)); err != nil {
			fmt.Fprintf(stderr, "package remove: %v\n", err)
			return 1
		}
	}
	lf.Remove(stack)
	if err := lockfile.Write(root, lf); err != nil {
		fmt.Fprintf(stderr, "package remove: %v\n", err)
		return 1
	}
	if *keepFiles {
		fmt.Fprintf(stdout, "removed %s from the lockfile (OPS/%s/ kept)\n", e.Name, stack)
	} else {
		fmt.Fprintf(stdout, "removed %s and deleted OPS/%s/\n", e.Name, stack)
	}
	return 0
}

// workspaceRootOrCwd finds the workspace root (nearest dir with OPS/), falling
// back to the cwd when there is no OPS/ yet.
func workspaceRootOrCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if root := findWorkspaceRoot(cwd); root != "" {
		return root
	}
	return cwd
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// editedCell renders the EDITED? column: "-" for vendor-only (edited==nil),
// "missing" when the stack dir is gone, else "yes"/"no".
func editedCell(edited *bool, present bool) string {
	if edited == nil {
		return "-"
	}
	if !present {
		return "missing"
	}
	if *edited {
		return "yes"
	}
	return "no"
}
