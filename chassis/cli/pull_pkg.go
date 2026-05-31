package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
)

// runPackagePull fetches a package into the workspace's .txco/vendor/ cache
// WITHOUT installing it into OPS/ or recording a lockfile entry. For offline
// inspection / pre-fetching.
//
//	txco package pull sales@v3
//	txco package pull oci://ghcr.io/you/support-basic:0.1.0
func runPackagePull(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("package pull", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco package pull <ref>

Fetch a package into .txco/vendor/<name>/<version>/ without installing it into
OPS/. Use `+"`txco install`"+` to materialize a package into a stack.
`)
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
		fmt.Fprintf(stderr, "package pull: %v\n", err)
		return 1
	}
	root := findWorkspaceRoot(cwd)
	if root == "" {
		root = cwd
	}

	dir, prov, cleanup, err := fetchPackage(resolvePackageRef(spec, workspaceRegistry(root)))
	if err != nil {
		fmt.Fprintf(stderr, "package pull: %v\n", err)
		return 1
	}
	defer cleanup()

	m, err := manifest.ParseFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "package pull: no %s at package root (is %q a package?)\n", manifest.FileName, spec)
		} else {
			fmt.Fprintf(stderr, "package pull: %v\n", err)
		}
		return 1
	}

	dest := filepath.Join(root, ".txco", "vendor", m.Name, m.Version)
	if err := os.RemoveAll(dest); err != nil {
		fmt.Fprintf(stderr, "package pull: %v\n", err)
		return 1
	}
	if _, err := copyTree(dir, dest); err != nil {
		fmt.Fprintf(stderr, "package pull: %v\n", err)
		return 1
	}
	_, _ = ensureGitignored(root, ".txco/")

	if prov.Reference != "" {
		fmt.Fprintf(stdout, "pulled %s %s (%s) into %s\n", m.Name, m.Version, prov.Reference, displayPath(root, dest))
	} else {
		fmt.Fprintf(stdout, "pulled %s %s into %s\n", m.Name, m.Version, displayPath(root, dest))
	}
	return 0
}
