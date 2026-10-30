package javyplugin

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// TestVendoredPlugins: each vendored plugin is a valid module that declares
// the namespace this package says it does (Javy records it in an
// `import_namespace` custom section), exports what a dynamically linked op
// imports, and imports nothing but WASI — the sandbox posture the runtime
// relies on. Namespaces are distinct, and the current plugin is the one with
// the natives.
func TestVendoredPlugins(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCustomSections(true))
	defer rt.Close(ctx)

	seen := map[string]bool{}
	for i, p := range All() {
		if seen[p.Namespace] {
			t.Fatalf("namespace %q vendored twice", p.Namespace)
		}
		seen[p.Namespace] = true
		if i == 0 && (p.Namespace != Namespace || !p.Native || len(p.Wasm) != len(Bytes())) {
			t.Fatalf("All()[0] = %q native=%v, want the current plugin %q with natives", p.Namespace, p.Native, Namespace)
		}
		cm, err := rt.CompileModule(ctx, p.Wasm)
		if err != nil {
			t.Fatalf("%s: compile: %v", p.Namespace, err)
		}
		var declared string
		for _, cs := range cm.CustomSections() {
			if cs.Name() == "import_namespace" {
				declared = string(cs.Data())
			}
		}
		if declared != p.Namespace {
			t.Errorf("%s: plugin declares import_namespace %q", p.Namespace, declared)
		}
		exports := cm.ExportedFunctions()
		for _, name := range []string{"initialize-runtime", "invoke", "cabi_realloc"} {
			if _, ok := exports[name]; !ok {
				t.Errorf("%s: missing export %q", p.Namespace, name)
			}
		}
		if _, ok := cm.ExportedMemories()["memory"]; !ok {
			t.Errorf("%s: missing exported memory", p.Namespace)
		}
		for _, f := range cm.ImportedFunctions() {
			if m, n, _ := f.Import(); m != wasi_snapshot_preview1.ModuleName {
				t.Errorf("%s: imports %s.%s; a plugin may import only WASI", p.Namespace, m, n)
			}
		}
		_ = cm.Close(ctx)
	}
	if _, ok := Lookup(Namespace); !ok {
		t.Fatalf("Lookup(%q) = false", Namespace)
	}
	if _, ok := Lookup(legacyNamespace); !ok {
		t.Fatalf("Lookup(%q) = false", legacyNamespace)
	}
	if _, ok := Lookup("nope"); ok {
		t.Fatal("Lookup(nope) = true")
	}
}
