package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
)

// runInspect shows a package's identity and exports without installing it.
//
//	txco inspect dir:./examples/packages/support-basic
//	txco inspect github:loremlabs/txco-packages/support-basic [--json]
func runInspect(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("inspect", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit machine-readable JSON")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco package inspect <source> [--json]

Show a package's identity and exports without installing it.

Sources:
  sales@v3                         registry ref (default: registry.thanks.computer/txco)
  oci://ghcr.io/you/sales:v3       explicit OCI ref
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

	// Resolve a bare/namespaced registry ref against the workspace registry
	// config + baked defaults; explicit schemes/local paths pass through.
	dir, prov, cleanup, err := fetchPackage(resolvePackageRef(spec, workspaceRegistry(".")))
	if err != nil {
		fmt.Fprintf(stderr, "inspect: %v\n", err)
		return 1
	}
	defer cleanup()

	m, err := manifest.ParseFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "inspect: no %s at package root (is %q a package?)\n", manifest.FileName, spec)
		} else {
			fmt.Fprintf(stderr, "inspect: %v\n", err)
		}
		return 1
	}
	ops, _ := bundle.WalkFS(os.DirFS(dir), ".")
	stacks := summarizeStacks(ops)

	if *asJSON {
		out := struct {
			Manifest *manifest.Manifest `json:"manifest"`
			Resolved string             `json:"resolved,omitempty"`
			Stacks   []stackSummary     `json:"stacks"`
		}{m, prov.Reference, stacks}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "inspect: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "%s %s\n", m.Name, m.Version)
	if m.Summary != "" {
		fmt.Fprintf(stdout, "  %s\n", m.Summary)
	}
	if m.Package.Kind != "" {
		fmt.Fprintf(stdout, "  kind:          %s\n", m.Package.Kind)
	}
	if m.Compatibility.Txco != "" {
		fmt.Fprintf(stdout, "  compat:        txco %s\n", m.Compatibility.Txco)
	}
	if prov.Reference != "" {
		fmt.Fprintf(stdout, "  resolved:      %s\n", prov.Reference)
	}
	for _, s := range stacks {
		fmt.Fprintf(stdout, "  stack:         %s (%d scope%s, %d rule%s)\n",
			s.Name, s.Scopes, pluralS(s.Scopes), s.Rules, pluralS(s.Rules))
	}
	if len(m.Operations.Bundled) > 0 {
		names := make([]string, 0, len(m.Operations.Bundled))
		for _, b := range m.Operations.Bundled {
			names = append(names, b.Name)
		}
		fmt.Fprintf(stdout, "  bundled ops:   %s\n", strings.Join(names, ", "))
	}
	if len(m.Operations.Required) > 0 {
		names := make([]string, 0, len(m.Operations.Required))
		for _, r := range m.Operations.Required {
			if r.Kind != "" {
				names = append(names, fmt.Sprintf("%s (%s)", r.Name, r.Kind))
			} else {
				names = append(names, r.Name)
			}
		}
		fmt.Fprintf(stdout, "  required ops:  %s\n", strings.Join(names, ", "))
	}
	if len(m.Capabilities) > 0 {
		fmt.Fprintf(stdout, "  capabilities:  %s   (advisory — not enforced)\n", strings.Join(m.Capabilities, ", "))
	}
	return 0
}
