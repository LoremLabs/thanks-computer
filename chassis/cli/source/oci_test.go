package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
)

// TestOCIRoundTrip packs the example package into an in-process OCI store, then
// pulls it back via ociSource (with the repository factory pointed at that
// store). No registry, no network — the oras.Target abstraction lets a local
// oci.Store stand in for a remote repository.
func TestOCIRoundTrip(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "examples", "packages", "support-basic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "txco.package.yaml")); err != nil {
		t.Skipf("example package not found at %s: %v", fixture, err)
	}
	ctx := context.Background()

	// Publish into a local oci.Store.
	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layer, err := tarGzDir(fixture)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	mf, err := os.ReadFile(filepath.Join(fixture, "txco.package.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manDesc, err := packPackageArtifact(ctx, store, layer, mf, "0.1.0")
	if err != nil {
		t.Fatalf("pack: %v", err)
	}

	// Point the pull factory at that store.
	prev := SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { SetRepositoryFactory(prev) })

	// Pull by tag.
	src, err := newOCISource("oci://registry.thanks.computer/txco/support-basic:0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	n, err := src.Fetch(ctx, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n == 0 {
		t.Error("no files extracted")
	}
	for _, rel := range []string{
		"txco.package.yaml",
		"OPS/support/0100_TRIAGE/classify.txcl",
		"OPS/support/0100_TRIAGE/classify.js", // bundled compute rides along
		"OPS/support/0200_NOTIFY/notify.txcl",
	} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s after pull: %v", rel, err)
		}
	}

	// Provenance pinned to the published digest.
	prov := src.Resolved()
	if prov.Digest != manDesc.Digest.String() {
		t.Errorf("digest = %q, want %q", prov.Digest, manDesc.Digest.String())
	}
	if !strings.Contains(prov.Reference, "@sha256:") {
		t.Errorf("reference not digest-pinned: %q", prov.Reference)
	}
	if prov.Registry != "registry.thanks.computer" || prov.Namespace != "txco" || prov.Name != "support-basic" {
		t.Errorf("provenance = %+v", prov)
	}
}
