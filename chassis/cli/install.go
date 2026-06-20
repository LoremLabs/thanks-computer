package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
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
	as := fs.String("as", "", "namespace prefix to install the package's stack under (e.g. --as inbox → OPS/inbox/...); omitted, the package's own stack name is used")
	vendorOnly := fs.Bool("vendor-only", false, "fetch + validate into .txco/vendor/, do not touch OPS/")
	dryRun := fs.Bool("dry-run", false, "show what would change; mutate nothing")
	force := fs.Bool("force", false, "overwrite a tracked stack even if it has local edits")
	requireSig := fs.Bool("require-signature", false, "fail unless the package is signed by a trusted key")
	keyFlags := fs.StringArray("key", nil, "trusted public key (ssh-ed25519 line, .pub path, or base64); repeatable")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco install <source> [--as <namespace>] [--vendor-only] [--dry-run]

Materialize a package into OPS/, then review and run `+"`txco apply`"+` to deploy.
Install never contacts a chassis.

Sources:
  sales@v3                         registry ref (default: registry.thanks.computer/txco)
  acme/sales@v3                    namespaced registry ref
  oci://ghcr.io/you/sales:v3       explicit OCI ref (or @sha256:... to pin)
  github:owner/repo[@ref][/sub]    a package in a public GitHub repo
  dir:./path                       a local package directory

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

	// Resolve a bare/namespaced registry ref (sales@v3) to a concrete oci://
	// spec using the workspace registry config + baked defaults; explicit
	// schemes and local paths pass through.
	srcSpec := resolvePackageRef(spec, workspaceRegistry(root))

	// Fetch into a temp staging dir so a failed fetch never half-writes OPS/.
	staging, prov, cleanup, err := fetchPackage(srcSpec)
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

	// Signature verification — runs before any OPS/ write so a fail-closed
	// --require-signature aborts cleanly. Skipped silently for non-OCI sources
	// (dir:/github:) unless --require-signature forces a check (which they fail).
	signedBy := ""
	if prov.Digest != "" || *requireSig {
		trusted, err := loadTrustedKeys(root, *keyFlags, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "install: %v\n", err)
			return 1
		}
		verdict, err := verifyPackageSignature(prov, trusted)
		if err != nil {
			fmt.Fprintf(stderr, "install: signature check: %v\n", err)
			return 1
		}
		if !enforceSignaturePosture(verdict, *requireSig, stdout, stderr) {
			return 1
		}
		if verdict.Signed && verdict.Trusted {
			signedBy = verdict.KeyID
		}
	}

	if *vendorOnly {
		return installVendorOnly(spec, prov, m, staging, root, signedBy, *dryRun, stdout, stderr)
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
	// --as renames the package's BASE stack, preserving any trailing channel
	// segment (`_mail`/`_cron`). A normal stack renames (support → billing); a
	// channel-only package installs UNDER the chosen base (`_mail` → <as>/_mail)
	// instead of flattening the channel away. Omitted, the package's own stack
	// name is used as-is.
	base, channel := splitBaseChannel(exportedStack)
	if *as != "" {
		base = strings.Trim(*as, "/")
	}
	installedAs := path.Join(base, channel)
	if err := opname.ValidStack(installedAs); err != nil {
		fmt.Fprintf(stderr, "install: --as %q: %v\n", *as, err)
		return 1
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

	// Inspect the on-disk state of a stack we already own so we can (a) refuse to
	// clobber hand-edits without --force, and (b) decide whether the "no change"
	// short-circuit is safe.
	var ownedState string
	if owned != nil {
		st, err := stackEditState(root, *owned)
		if err != nil {
			fmt.Fprintf(stderr, "install: %v\n", err)
			return 1
		}
		ownedState = st
		// Refuse to overwrite hand-edits made since install unless --force —
		// same guard as `txco package upgrade`. Applies in --dry-run too (it
		// reflects what a real run would do); `--force --dry-run` previews.
		if st == "edited" && !*force {
			fmt.Fprintf(stderr, "install: OPS/%s/ has local edits since install; review with `txco diff` or re-run with --force\n", installedAs)
			return 1
		}
	}

	// "No change" only when the on-disk stack is clean AND the package hash is
	// unchanged. An edited (with --force) or missing stack must re-materialize,
	// so the short-circuit must not swallow it.
	if owned != nil && ownedState == "clean" && owned.ManifestHash == newHash && !*dryRun {
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
		warnBundledComputes(m, staging, stdout)
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
		Ref:           spec, // exactly what the user typed
		Registry:      prov.Registry,
		Namespace:     prov.Namespace,
		Name:          m.Name,
		Version:       m.Version,
		Resolved:      prov.Reference, // oci://…@sha256: (blank for dir:/github:)
		ExportedStack: exportedStack,
		InstalledAs:   installedAs,
		Mode:          "as-stack",
		ManifestHash:  newHash,
		SignedBy:      signedBy,
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
	warnBundledComputes(m, staging, stdout)
	printRequiredOpStubs(stdout, m.Operations.Required)
	fmt.Fprintf(stdout, "\nReview OPS/%s/, then run `txco apply` to deploy.\n", installedAs)
	return 0
}

// installVendorOnly fetches + validates a package into .txco/vendor/ without
// touching OPS/. For offline inspection / reusable op-packs.
func installVendorOnly(spec string, prov source.Provenance, m *manifest.Manifest, staging, root, signedBy string, dryRun bool, stdout, stderr io.Writer) int {
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
		Registry:    prov.Registry,
		Namespace:   prov.Namespace,
		Name:        m.Name,
		Version:     m.Version,
		Resolved:    prov.Reference,
		Mode:        "vendor-only",
		SignedBy:    signedBy,
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
// `txco apply` — but only for bundled computes shipped as .js source WITHOUT a
// prebuilt <name>.wasm sibling (in stagingDir). A package published with
// prebuilt wasm applies with no toolchain, so it draws no warning. Install
// itself never builds — it only materializes what was shipped.
func warnBundledComputes(m *manifest.Manifest, stagingDir string, w io.Writer) {
	needBuild := 0
	for _, b := range m.Operations.Bundled {
		wasmRel := strings.TrimSuffix(b.Path, filepath.Ext(b.Path)) + ".wasm"
		if _, err := os.Stat(filepath.Join(stagingDir, wasmRel)); err != nil {
			needBuild++
		}
	}
	if needBuild == 0 {
		return
	}
	// `txco apply` builds these from source via op.BuildFile, which
	// auto-fetches the pinned javy toolchain on first use — so this is just
	// a heads-up (a one-time ~11 MB download may happen), not a prerequisite.
	if _, err := exec.LookPath("javy"); err != nil {
		fmt.Fprintf(w, "  note: ships %d bundled compute(s) without prebuilt wasm; `txco apply` will compile them, fetching the javy toolchain automatically on first build.\n", needBuild)
	}
}
