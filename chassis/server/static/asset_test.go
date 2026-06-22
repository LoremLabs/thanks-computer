package static

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func assetWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("FILES/robots.txt", "CHASSIS")
	mk("OPS/hello/FILES/robots.txt", "STACK")
	mk("OPS/hello/FILES/_mail/welcome.html", "HELLO-MAIL") // _-private
	mk("OPS/hello/FILES/about.html", "<about>")
	return root
}

// Asset resolves an EXACT path across the same layers as Lookup, reaches
// `_`-private assets (unlike HTTP serving), and does NOT try_files-probe.
func TestIndexAsset(t *testing.T) {
	ix := NewIndex(assetWorkspace(t), zap.NewNop())

	// The headline: a `_`-private template is readable as DATA, even though
	// HTTP serving (staticResultBody) would refuse it.
	if r, ok := ix.Asset("", "hello", "_mail/welcome.html"); !ok || string(r.Body) != "HELLO-MAIL" {
		t.Fatalf("private _mail asset must be readable; ok=%v %+v", ok, r)
	}

	// Operator/stack layer precedence over chassis-wide.
	if r, ok := ix.Asset("", "hello", "robots.txt"); !ok || string(r.Body) != "STACK" {
		t.Fatalf("routed stack robots; ok=%v %+v", ok, r)
	}
	if r, ok := ix.Asset("", "", "robots.txt"); !ok || string(r.Body) != "CHASSIS" {
		t.Fatalf("unrouted → chassis robots; ok=%v %+v", ok, r)
	}

	// Exact-only: "about" must NOT resolve to about.html (no try_files).
	if _, ok := ix.Asset("", "hello", "about"); ok {
		t.Fatalf("Asset must not try_files-probe (about → about.html)")
	}
	if r, ok := ix.Asset("", "hello", "about.html"); !ok || string(r.Body) != "<about>" {
		t.Fatalf("exact about.html must resolve; ok=%v %+v", ok, r)
	}

	// Traversal rejected; plain miss is a clean (_, false).
	if _, ok := ix.Asset("", "hello", "../../etc/passwd"); ok {
		t.Fatalf("traversal must be rejected")
	}
	if _, ok := ix.Asset("", "hello", "nope.txt"); ok {
		t.Fatalf("missing file must miss")
	}
}

// A tenant FILES/ asset surfaces through Asset as a CAS entry (Hash set,
// no inline Body) — including `_`-private templates — so read-file can
// resolve the bytes from the filecas store on data-plane nodes.
func TestIndexAssetTenantCAS(t *testing.T) {
	db := tenantDB(t)
	insTenant(t, db, "tnt_a", "acme", false)
	insStack(t, db, "s_a", "tnt_a", "web", 10)
	insFile(t, db, 10, "FILES/_mail/welcome.html", "HELLO", hhex("HELLO"))

	ix := NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatalf("RebuildTenant: %v", err)
	}

	r, ok := ix.Asset("acme", "web", "_mail/welcome.html")
	if !ok || !r.Found {
		t.Fatalf("tenant private asset must resolve; ok=%v %+v", ok, r)
	}
	if r.Hash != hhex("HELLO") {
		t.Fatalf("tenant asset must carry CAS hash; got %q", r.Hash)
	}
	if r.Body != nil {
		t.Fatalf("tenant CAS entry must carry no inline body; %+v", r)
	}
}
