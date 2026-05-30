package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/opname"
)

// runInstall materializes a package into the local OPS/ tree, records it in
// the lockfile, and stops — the user reviews the files and runs `txco apply`
// to deploy. Install never contacts a chassis.
//
//	txco install dir:./examples/packages/support-basic --as support
//	txco install github:loremlabs/txco-packages/support-basic --as support
//	txco install dir:./pkg --vendor-only
//	txco install dir:./pkg --as support --dry-run
func runInstall(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("install", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	as := fs.String("as", "", "materialize the package's single stack as <stack>")
	vendorOnly := fs.Bool("vendor-only", false, "fetch + validate into .txco/vendor/, do not touch OPS/")
	dryRun := fs.Bool("dry-run", false, "show what would change; mutate nothing")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco install <source> [--as <stack>] [--vendor-only] [--dry-run]

Materialize a package into OPS/, then review and run `+"`txco apply`"+` to deploy.
Install never contacts a chassis.

Sources:
  dir:./path                       a local package directory
  github:owner/repo[@ref][/sub]    a package in a public GitHub repo

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	spec := fs.Arg(0)

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	root := findWorkspaceRoot(cwd)
	if root == "" {
		root = cwd // bootstrap a fresh workspace at cwd
	}

	// Fetch into a temp staging dir so a failed fetch never half-writes OPS/.
	staging, cleanup, err := fetchPackage(spec)
	if err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	defer cleanup()

	m, err := manifest.ParseFile(filepath.Join(staging, manifest.FileName))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "install: no %s at package root (is %q a package?)\n", manifest.FileName, spec)
		} else {
			fmt.Fprintf(stderr, "install: %v\n", err)
		}
		return 1
	}
	if probs := manifest.Validate(m, os.DirFS(staging), "."); len(probs) > 0 {
		fmt.Fprintf(stderr, "install: %s has %d problem%s:\n", nameOr(m.Name, "package"), len(probs), pluralS(len(probs)))
		for _, p := range probs {
			fmt.Fprintf(stderr, "  %v\n", p)
		}
		return 1
	}

	if *vendorOnly {
		return installVendorOnly(spec, m, staging, root, *dryRun, stdout, stderr)
	}

	ops, err := bundle.WalkFS(os.DirFS(staging), ".")
	if err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	exported := exportedStackNames(ops)
	if len(exported) != 1 {
		fmt.Fprintf(stderr, "install: package exports %d stacks (%s); v1 install supports single-stack packages only\n",
			len(exported), strings.Join(exported, ", "))
		return 1
	}
	exportedStack := exported[0]
	installedAs := exportedStack
	if *as != "" {
		installedAs = strings.Trim(*as, "/")
		if err := opname.ValidStack(installedAs); err != nil {
			fmt.Fprintf(stderr, "install: --as %q: %v\n", *as, err)
			return 1
		}
	}

	stackFiles := opsToFiles(ops) // <scope>/<name>.txcl — stack-independent; for the manifest hash
	newHash := localManifestHash(stackFiles)
	srcStackDir := filepath.Join(staging, "OPS", exportedStack)
	destStackDir := filepath.Join(root, "OPS", installedAs)

	lf, err := lockfile.Read(root)
	if err != nil {
		fmt.Fprintf(stderr, "install: read lockfile: %v\n", err)
		return 1
	}
	owned := lf.FindStack(installedAs)

	if owned != nil && owned.ManifestHash == newHash && !*dryRun {
		fmt.Fprintf(stdout, "%s %s already installed as %q (no change)\n", m.Name, m.Version, installedAs)
		return 0
	}

	if *dryRun {
		fmt.Fprintf(stdout, "install (dry-run): %s %s\n", m.Name, m.Version)
		fmt.Fprintf(stdout, "  would materialize OPS/%s/ (from package stack %q):\n", installedAs, exportedStack)
		rels, _ := listTreeRel(srcStackDir)
		for _, rel := range rels {
			fmt.Fprintf(stdout, "    OPS/%s/%s\n", installedAs, rel)
		}
		warnBundledComputes(m, stdout)
		printRequiredOpStubs(stdout, m.Operations.Required)
		fmt.Fprintf(stdout, "\n  would update %s\n(dry-run; nothing written)\n", lockfile.FileName)
		return 0
	}

	// Materialize. A stack we already own (in the lockfile) is safe to replace;
	// an untracked populated stack dir is refused.
	if owned == nil {
		if err := ensureEmptyOrCreate(destStackDir); err != nil {
			fmt.Fprintf(stderr, "install: %v\n  (OPS/%s/ has content not from a tracked package — remove it or choose another --as)\n", err, installedAs)
			return 1
		}
	} else if err := os.RemoveAll(destStackDir); err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	nCopied, err := copyTree(srcStackDir, destStackDir)
	if err != nil {
		fmt.Fprintf(stderr, "install: materialize: %v\n", err)
		return 1
	}

	lf.Upsert(lockfile.Entry{
		Ref:           spec,
		Name:          m.Name,
		Version:       m.Version,
		ExportedStack: exportedStack,
		InstalledAs:   installedAs,
		Mode:          "as-stack",
		ManifestHash:  newHash,
		InstalledAt:   lockfile.Now(),
	})
	if err := lockfile.Write(root, lf); err != nil {
		fmt.Fprintf(stderr, "install: write lockfile: %v\n", err)
		return 1
	}

	verb := "installed"
	if owned != nil {
		verb = "updated"
	}
	fmt.Fprintf(stdout, "%s %s %s as stack %q (%d file%s)\n",
		verb, m.Name, m.Version, installedAs, nCopied, pluralS(nCopied))
	if len(m.Capabilities) > 0 {
		fmt.Fprintf(stdout, "  requests (advisory, not enforced): %s\n", strings.Join(m.Capabilities, ", "))
	}
	warnBundledComputes(m, stdout)
	printRequiredOpStubs(stdout, m.Operations.Required)
	fmt.Fprintf(stdout, "\nReview OPS/%s/, then run `txco apply` to deploy.\n", installedAs)
	return 0
}

// installVendorOnly fetches + validates a package into .txco/vendor/ without
// touching OPS/. For offline inspection / reusable op-packs.
func installVendorOnly(spec string, m *manifest.Manifest, staging, root string, dryRun bool, stdout, stderr io.Writer) int {
	dest := filepath.Join(root, ".txco", "vendor", m.Name, m.Version)
	if dryRun {
		fmt.Fprintf(stdout, "install --vendor-only (dry-run): would vendor %s %s into %s (no OPS/ change)\n",
			m.Name, m.Version, displayPath(root, dest))
		return 0
	}
	if err := os.RemoveAll(dest); err != nil {
		fmt.Fprintf(stderr, "install: %v\n", err)
		return 1
	}
	if _, err := copyTree(staging, dest); err != nil {
		fmt.Fprintf(stderr, "install: vendor: %v\n", err)
		return 1
	}
	_, _ = ensureGitignored(root, ".txco/")

	lf, err := lockfile.Read(root)
	if err != nil {
		fmt.Fprintf(stderr, "install: read lockfile: %v\n", err)
		return 1
	}
	lf.Upsert(lockfile.Entry{
		Ref:         spec,
		Name:        m.Name,
		Version:     m.Version,
		Mode:        "vendor-only",
		InstalledAt: lockfile.Now(),
	})
	if err := lockfile.Write(root, lf); err != nil {
		fmt.Fprintf(stderr, "install: write lockfile: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "vendored %s %s into %s (no OPS/ change)\n", m.Name, m.Version, displayPath(root, dest))
	return 0
}

// warnBundledComputes notes the javy-on-PATH requirement for the user's later
// `txco apply` when a package ships bundled computes. Install itself never
// builds — it only materializes the .js source.
func warnBundledComputes(m *manifest.Manifest, w io.Writer) {
	if len(m.Operations.Bundled) == 0 {
		return
	}
	if _, err := exec.LookPath("javy"); err != nil {
		fmt.Fprintf(w, "  note: ships %d bundled compute(s); `txco apply` needs `javy` on PATH to build them.\n", len(m.Operations.Bundled))
	}
}
