package cli

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// TestLocalManifestHashMatchesServer pins the CLI's localManifestHash
// to the chassis-side computeManifestHash. The golden value below was
// observed live: it's the manifest_hash the chassis stored for
// boot/web v14 with a single file at "0/resonator.txcl" whose content
// is the two-line WHEN/EXEC rule.
//
// If chassis/server/admin/stacks.go:computeManifestHash ever changes
// shape (different separator, different hash, sort order), this test
// will fail loudly so the CLI can be kept in lockstep.
func TestLocalManifestHashMatchesServer(t *testing.T) {
	files := []client.StackFile{
		{
			Path:    "0/resonator.txcl",
			Content: "WHEN ._txc.src == \"http\"\nEXEC \"hello-world/0\"\n",
		},
	}
	const golden = "6530c71780bb3a0bff2105d9950294da3e5ad40758b639baa8ea663ad27c07bb"
	got := localManifestHash(files)
	if got != golden {
		t.Errorf("localManifestHash drift vs chassis-side computeManifestHash:\n got:  %s\n want: %s", got, golden)
	}
}

// TestLocalManifestHashStableOrder — input order must not affect the
// output. Mirrors the chassis-side sort step (stacks.go:185).
func TestLocalManifestHashStableOrder(t *testing.T) {
	a := []client.StackFile{
		{Path: "100/hello.txcl", Content: "EXEC \"x\"\n"},
		{Path: "100/world.txcl", Content: "EXEC \"y\"\n"},
		{Path: "200/sort.txcl", Content: "EXEC \"z\"\n"},
	}
	b := []client.StackFile{
		{Path: "200/sort.txcl", Content: "EXEC \"z\"\n"},
		{Path: "100/world.txcl", Content: "EXEC \"y\"\n"},
		{Path: "100/hello.txcl", Content: "EXEC \"x\"\n"},
	}
	if localManifestHash(a) != localManifestHash(b) {
		t.Errorf("manifest hash depends on input order; got %q vs %q",
			localManifestHash(a), localManifestHash(b))
	}
}

// TestLocalManifestHashSensitiveToContent — same paths, different
// content must produce different hashes.
func TestLocalManifestHashSensitiveToContent(t *testing.T) {
	a := []client.StackFile{{Path: "0/r.txcl", Content: "x"}}
	b := []client.StackFile{{Path: "0/r.txcl", Content: "y"}}
	if localManifestHash(a) == localManifestHash(b) {
		t.Errorf("manifest hash collides on differing content")
	}
}

// TestLocalManifestHashEmpty — empty file set hashes to sha256("").
// Same edge case the chassis comments call out.
func TestLocalManifestHashEmpty(t *testing.T) {
	got := localManifestHash(nil)
	const sha256Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != sha256Empty {
		t.Errorf("empty file set hash = %q, want %q", got, sha256Empty)
	}
}
