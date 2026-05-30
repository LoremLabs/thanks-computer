package manifest

import (
	"strings"
	"testing"
	"testing/fstest"
)

// validPackage returns a well-formed manifest + tree. Tests clone via a fresh
// call, then mutate.
func validPackage() (*Manifest, fstest.MapFS) {
	fsys := fstest.MapFS{
		"OPS/support/0100/classify.txcl": {Data: []byte(`EXEC "op://classify"`)},
		"OPS/support/0100/classify.js":   {Data: []byte(`export default () => ({})`)},
		"OPS/support/0200/notify.txcl":   {Data: []byte(`EXEC "op://NOTIFY"`)},
	}
	m := &Manifest{
		APIVersion: APIVersion,
		Kind:       Kind,
		Name:       "support-basic",
		Version:    "0.1.0",
		Package:    PackageSpec{Kind: "department"},
		Operations: Operations{
			Bundled:  []BundledOp{{Name: "classify", Path: "OPS/support/0100/classify.js", Lang: "js"}},
			Required: []RequiredOp{{Name: "NOTIFY", Kind: "http"}},
		},
	}
	return m, fsys
}

func TestValidateValid(t *testing.T) {
	m, fsys := validPackage()
	if errs := Validate(m, fsys, "."); len(errs) != 0 {
		t.Fatalf("expected valid, got: %v", errs)
	}
}

func TestValidateInvalid(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *Manifest, fsys fstest.MapFS)
		want   string
	}{
		{"bad apiVersion", func(m *Manifest, _ fstest.MapFS) { m.APIVersion = "txco.dev/v1" }, "apiVersion"},
		{"bad kind", func(m *Manifest, _ fstest.MapFS) { m.Kind = "Departement" }, "kind"},
		{"non-semver", func(m *Manifest, _ fstest.MapFS) { m.Version = "v3" }, "semver"},
		{"bad package.kind", func(m *Manifest, _ fstest.MapFS) { m.Package.Kind = "widget" }, "package.kind"},
		{"required is actually bundled", func(m *Manifest, _ fstest.MapFS) {
			m.Operations.Bundled = nil
			m.Operations.Required = []RequiredOp{{Name: "classify"}, {Name: "NOTIFY"}}
		}, "colocated compute"},
		{"bundled file missing", func(m *Manifest, _ fstest.MapFS) {
			m.Operations.Bundled[0].Path = "OPS/support/0100/missing.js"
		}, "not found"},
		{"bundled wrong ext", func(m *Manifest, fsys fstest.MapFS) {
			fsys["OPS/support/0100/classify.txt"] = &fstest.MapFile{Data: []byte("x")}
			m.Operations.Bundled[0].Path = "OPS/support/0100/classify.txt"
		}, "must end in .js or .ts"},
		{"undeclared external ref", func(m *Manifest, _ fstest.MapFS) {
			m.Operations.Required = nil
		}, "not declared under operations.required"},
		{"unparseable txcl", func(_ *Manifest, fsys fstest.MapFS) {
			fsys["OPS/support/0300/bad.txcl"] = &fstest.MapFile{Data: []byte("EXEC")}
		}, "txcl"},
		{"empty OPS", func(_ *Manifest, fsys fstest.MapFS) {
			delete(fsys, "OPS/support/0100/classify.txcl")
			delete(fsys, "OPS/support/0100/classify.js")
			delete(fsys, "OPS/support/0200/notify.txcl")
		}, "no rules under OPS/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, fsys := validPackage()
			tc.mutate(m, fsys)
			errs := Validate(m, fsys, ".")
			if len(errs) == 0 {
				t.Fatalf("expected an error containing %q, got none", tc.want)
			}
			joined := errsJoin(errs)
			if !strings.Contains(joined, tc.want) {
				t.Errorf("errors %q do not contain %q", joined, tc.want)
			}
		})
	}
}

func errsJoin(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error())
		b.WriteString("\n")
	}
	return b.String()
}
