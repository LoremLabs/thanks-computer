package manifest

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

var semverRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// Validate checks a manifest against the package tree rooted at root within
// fsys (use os.DirFS(pkgRoot) + "." for an on-disk package). It accumulates
// ALL problems rather than failing fast, mirroring the apply validate-loop's
// report style, so `txco package validate` can show everything at once.
//
// The semantic core is the op-resolution contract (design §4): every op:// ref
// is classified as bundled (a colocated <name>.js/.ts sibling exists, exactly
// as apply's resolveOpRefsColocated decides) or external (none). The manifest
// must declare each accordingly.
func Validate(m *Manifest, fsys fs.FS, root string) []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	// 1. Header.
	if m.APIVersion != APIVersion {
		add("apiVersion: must be %q, got %q", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		add("kind: must be %q, got %q", Kind, m.Kind)
	}

	// 2. Identity (name + version only; provenance lives in the ref/lockfile).
	if m.Name == "" {
		add("name: required")
	} else if strings.Contains(m.Name, "/") {
		if err := opname.ValidStack(m.Name); err != nil { // hierarchical names allowed
			add("name: %v", err)
		}
	} else if err := opname.Valid(m.Name); err != nil {
		add("name: %v", err)
	}
	if !semverRE.MatchString(m.Version) {
		add("version: %q is not semver (MAJOR.MINOR.PATCH)", m.Version)
	}
	if k := m.Package.Kind; k != "" {
		switch k {
		case "department", "stack-template", "op-pack":
		default:
			add("package.kind: %q not one of department|stack-template|op-pack", k)
		}
	}
	if dm := m.Package.Install.DefaultMode; dm != "" {
		switch dm {
		case "as-stack", "into-stack", "vendor-only":
		default:
			add("package.install.defaultMode: %q not one of as-stack|into-stack|vendor-only", dm)
		}
	}

	// 3. Walk the OPS tree.
	ops, werr := bundle.WalkFS(fsys, root)
	if werr != nil {
		add("walk OPS/: %v", werr)
		return errs // can't proceed without the tree
	}
	if len(ops) == 0 {
		add("package has no rules under OPS/")
	}

	// 4/5. Per-rule parse + name checks; classify every op:// ref as bundled
	// (colocated compute present) or external.
	bundledRefs := map[string]bool{}
	externalRefs := map[string]bool{}
	for _, op := range ops {
		if err := opname.ValidStack(op.Stack); err != nil {
			add("%s: stack: %v", op.SourcePath, err)
		}
		if err := opname.Valid(op.Name); err != nil {
			add("%s: name: %v", op.SourcePath, err)
		}
		if _, perr := txcl.Resonator(op.Txcl); perr != nil {
			add("%s: txcl: %v", op.SourcePath, perr)
		}
		dir := path.Dir(op.SourcePath)
		for _, name := range oprefs.References(op.Txcl) {
			if colocatedExists(fsys, dir, name) {
				bundledRefs[name] = true
			} else {
				externalRefs[name] = true
			}
		}
	}

	// 6. Bundled ops: declared file exists + correct extension; the op is
	//    actually referenced with a colocated compute.
	declaredBundled := map[string]bool{}
	for _, b := range m.Operations.Bundled {
		if b.Name == "" || b.Path == "" {
			add("operations.bundled: each entry needs name and path")
			continue
		}
		declaredBundled[b.Name] = true
		ext := strings.ToLower(path.Ext(b.Path))
		if ext != ".js" && ext != ".ts" {
			add("operations.bundled[%s]: path %q must end in .js or .ts", b.Name, b.Path)
		} else if b.Lang != "" && b.Lang != strings.TrimPrefix(ext, ".") {
			add("operations.bundled[%s]: lang %q does not match %s", b.Name, b.Lang, ext)
		}
		if _, err := fs.Stat(fsys, b.Path); err != nil {
			add("operations.bundled[%s]: file %q not found", b.Name, b.Path)
		}
		if !bundledRefs[b.Name] {
			add("operations.bundled[%s]: not referenced as op://%s with a colocated compute", b.Name, b.Name)
		}
	}

	// 7. Required ops: each must be an EXTERNAL ref (referenced, no colocated).
	declaredRequired := map[string]bool{}
	for _, r := range m.Operations.Required {
		if r.Name == "" {
			add("operations.required: each entry needs a name")
			continue
		}
		declaredRequired[r.Name] = true
		if r.Kind != "" && r.Kind != "http" && r.Kind != "mcp" {
			add("operations.required[%s]: kind %q not http|mcp", r.Name, r.Kind)
		}
		if !externalRefs[r.Name] {
			if bundledRefs[r.Name] {
				add("operations.required[%s]: has a colocated compute — declare it under operations.bundled, not required", r.Name)
			} else {
				add("operations.required[%s]: never referenced as op://%s in any rule", r.Name, r.Name)
			}
		}
	}

	// 8. Completeness: every referenced op must be declared, so install can
	//    surface external requirements and inspect can report bundled ones.
	for name := range externalRefs {
		if !declaredRequired[name] {
			add("op://%s is referenced with no colocated compute but is not declared under operations.required", name)
		}
	}
	for name := range bundledRefs {
		if !declaredBundled[name] {
			add("op://%s has a colocated compute but is not declared under operations.bundled", name)
		}
	}

	return errs
}

// colocatedExists reports whether a <name>.js or <name>.ts sibling exists in
// dir within fsys — the same rule apply's resolveOpRefsColocated uses to
// decide whether op://name resolves to a bundled compute.
func colocatedExists(fsys fs.FS, dir, name string) bool {
	for _, ext := range []string{".js", ".ts"} {
		if _, err := fs.Stat(fsys, path.Join(dir, name+ext)); err == nil {
			return true
		}
	}
	return false
}
