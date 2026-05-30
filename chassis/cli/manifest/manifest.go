// Package manifest models txco.package.yaml — a TxCo package's identity,
// compatibility, op-resolution contract, and advisory metadata — and
// validates a package tree against it.
//
// The Go struct + Validate are AUTHORITATIVE (the JSON Schema shipped under
// docs/ is editor-tooling only): the meaningful checks are semantic — does
// each bundled compute file exist? does every required op appear as an
// EXTERNAL op:// ref (no colocated compute)? does every rule parse? — and
// cannot be expressed in JSON Schema.
package manifest

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// FileName is the manifest filename at a package root.
const FileName = "txco.package.yaml"

// APIVersion and Kind are the only accepted header values in v1alpha1.
// (thanks.computer is the project domain group.)
const (
	APIVersion = "thanks.computer/v1alpha1"
	Kind       = "Package"
)

// Manifest is the parsed txco.package.yaml. The manifest carries IDENTITY
// only (name + version); a package's registry/namespace/publisher are
// provenance derived from the resolved ref and recorded in the lockfile, so a
// copied package cannot lie about its origin.
type Manifest struct {
	APIVersion    string        `yaml:"apiVersion" json:"apiVersion"`
	Kind          string        `yaml:"kind" json:"kind"`
	Name          string        `yaml:"name" json:"name"`
	Version       string        `yaml:"version" json:"version"`
	Summary       string        `yaml:"summary,omitempty" json:"summary,omitempty"`
	Package       PackageSpec   `yaml:"package,omitempty" json:"package,omitempty"`
	Compatibility Compatibility `yaml:"compatibility,omitempty" json:"compatibility,omitempty"`
	Operations    Operations    `yaml:"operations,omitempty" json:"operations,omitempty"`
	Build         Build         `yaml:"build,omitempty" json:"build,omitempty"`
	Capabilities  []string      `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Inlets        []Inlet       `yaml:"inlets,omitempty" json:"inlets,omitempty"`
	Requires      Requires      `yaml:"requires,omitempty" json:"requires,omitempty"` // reserved (Phase 4)
	Metadata      Metadata      `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

type PackageSpec struct {
	Kind    string      `yaml:"kind,omitempty" json:"kind,omitempty"` // department | stack-template | op-pack
	Install InstallSpec `yaml:"install,omitempty" json:"install,omitempty"`
}

type InstallSpec struct {
	DefaultMode    string `yaml:"defaultMode,omitempty" json:"defaultMode,omitempty"`       // as-stack | into-stack | vendor-only
	SuggestedStack string `yaml:"suggestedStack,omitempty" json:"suggestedStack,omitempty"` // default for --as
}

type Compatibility struct {
	Txco string `yaml:"txco,omitempty" json:"txco,omitempty"` // semver constraint; advisory in v1
}

// Operations is the load-bearing op-resolution contract: which op:// refs ride
// along as colocated computes (bundled) vs which are external endpoints the
// installer must wire into txco.yaml (required).
type Operations struct {
	Bundled  []BundledOp  `yaml:"bundled,omitempty" json:"bundled,omitempty"`
	Required []RequiredOp `yaml:"required,omitempty" json:"required,omitempty"`
}

type BundledOp struct {
	Name string `yaml:"name" json:"name"`
	Path string `yaml:"path" json:"path"`                     // OPS/.../<name>.js — relative to the package root
	Lang string `yaml:"lang,omitempty" json:"lang,omitempty"` // js | ts
}

type RequiredOp struct {
	Name        string `yaml:"name" json:"name"`
	Kind        string `yaml:"kind,omitempty" json:"kind,omitempty"` // http | mcp (advisory)
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Example     string `yaml:"example,omitempty" json:"example,omitempty"`
}

type Build struct {
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"` // advisory toolchain, e.g. "javy >= 1.0"
}

type Inlet struct {
	Type               string `yaml:"type" json:"type"`
	SuggestedLocalPart string `yaml:"suggestedLocalPart,omitempty" json:"suggestedLocalPart,omitempty"`
	Description        string `yaml:"description,omitempty" json:"description,omitempty"`
}

type Requires struct {
	Packages []RequiredPackage `yaml:"packages,omitempty" json:"packages,omitempty"`
}

type RequiredPackage struct {
	Ref  string `yaml:"ref" json:"ref"`
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

type Metadata struct {
	Homepage string `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	Source   string `yaml:"source,omitempty" json:"source,omitempty"`
	License  string `yaml:"license,omitempty" json:"license,omitempty"`
}

// Parse unmarshals manifest bytes. Shape/syntax errors only; semantic checks
// are in Validate.
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileName, err)
	}
	return &m, nil
}

// ParseFile reads and parses a manifest from an on-disk path.
func ParseFile(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}
