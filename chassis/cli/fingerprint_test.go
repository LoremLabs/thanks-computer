package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// mustFingerprint runs stackSourceFingerprint and fails the test on error or an
// empty digest. The dev watch loop skips a stack when its fingerprint is
// unchanged, so the load-bearing property these tests guard is: a real source
// change MUST move the digest (else an edit is silently dropped), while inert
// churn (input order, OS cruft) MUST NOT (else every stack re-pushes).
func mustFingerprint(t *testing.T, ops []bundle.Op, stackDir string) string {
	t.Helper()
	fp, err := stackSourceFingerprint(ops, stackDir)
	if err != nil {
		t.Fatalf("stackSourceFingerprint: %v", err)
	}
	if fp == "" {
		t.Fatal("stackSourceFingerprint returned an empty digest")
	}
	return fp
}

// writeAsset writes <stackDir>/<rel> (creating parents) and stamps a fixed
// mtime, so the stat-based asset half (path+size+mtime) is deterministic across
// runs rather than depending on wall-clock / filesystem mtime granularity.
func writeAsset(t *testing.T, stackDir, rel, content string, mtime time.Time) {
	t.Helper()
	full := filepath.Join(stackDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestStackSourceFingerprintOps covers the op half (the in-memory .txcl text).
// Identical ops → identical fp; input order is irrelevant (opsToFiles sorts);
// any edited op text or added op moves the fp. The two "did not change" asserts
// are the important ones: a fingerprint that fails to move on a real edit means
// you save and nothing pushes.
func TestStackSourceFingerprintOps(t *testing.T) {
	dir := t.TempDir() // no asset trees — exercises the op half alone
	base := []bundle.Op{
		{Scope: 100, Name: "a", Txcl: "EMIT .x = 1"},
		{Scope: 200, Name: "b", Txcl: "EMIT .y = 2"},
	}

	fp1 := mustFingerprint(t, base, dir)

	// Stable: recomputing identical ops yields the same digest.
	if fp2 := mustFingerprint(t, base, dir); fp1 != fp2 {
		t.Fatalf("identical ops produced different fp: %s vs %s", fp1, fp2)
	}

	// Order-independent: opsToFiles sorts by path, so input order can't matter.
	shuffled := []bundle.Op{base[1], base[0]}
	if fp := mustFingerprint(t, shuffled, dir); fp != fp1 {
		t.Fatalf("op input order changed fp: %s vs %s", fp, fp1)
	}

	// Edited op text → fp MUST change (else a real edit is silently dropped).
	edited := []bundle.Op{base[0], {Scope: 200, Name: "b", Txcl: "EMIT .y = 3"}}
	if fp := mustFingerprint(t, edited, dir); fp == fp1 {
		t.Fatal("edited op text did not change fp — a real edit would be silently dropped")
	}

	// Added op → fp MUST change.
	added := append(append([]bundle.Op{}, base...),
		bundle.Op{Scope: 300, Name: "c", Txcl: "EMIT .z = 4"})
	if fp := mustFingerprint(t, added, dir); fp == fp1 {
		t.Fatal("added op did not change fp")
	}
}

// TestStackSourceFingerprintAssets covers the asset half: a STAT-only walk of
// FILES/, VECTORS/, and KV/. A new asset, a size change, and an mtime bump each
// move the fp; a dotfile is ignored; all three trees are walked.
func TestStackSourceFingerprintAssets(t *testing.T) {
	dir := t.TempDir()
	ops := []bundle.Op{{Scope: 100, Name: "a", Txcl: "EMIT .x = 1"}}
	t0 := time.Unix(1_700_000_000, 0)
	t1 := time.Unix(1_700_000_100, 0)

	// Baseline: one asset in each of the three walked trees.
	writeAsset(t, dir, "FILES/app/index.html", "<html>", t0)
	writeAsset(t, dir, storeseed.DirVectors+"/v.jsonl", "vec", t0)
	writeAsset(t, dir, storeseed.DirKV+"/k.json", "kv", t0)
	base := mustFingerprint(t, ops, dir)

	// Stable recompute (no change on disk).
	if fp := mustFingerprint(t, ops, dir); fp != base {
		t.Fatalf("identical assets produced different fp: %s vs %s", fp, base)
	}

	// New asset under FILES/ → change.
	writeAsset(t, dir, "FILES/app/new.css", "body{}", t0)
	withNew := mustFingerprint(t, ops, dir)
	if withNew == base {
		t.Fatal("new FILES/ asset did not change fp")
	}

	// Size change only (same path + mtime, more bytes) → change. Confirms size
	// is in the digest.
	writeAsset(t, dir, "FILES/app/new.css", "body{color:red}", t0)
	sized := mustFingerprint(t, ops, dir)
	if sized == withNew {
		t.Fatal("asset size change did not change fp")
	}

	// mtime bump only (identical bytes, newer mtime) → change. Documents that
	// the asset half is stat-based: a rebuilt-but-identical file still re-pushes
	// (a deliberate trade-off vs. reading every asset's content per fire).
	writeAsset(t, dir, "FILES/app/new.css", "body{color:red}", t1)
	if fp := mustFingerprint(t, ops, dir); fp == sized {
		t.Fatal("asset mtime change did not change fp")
	}

	// A dotfile (e.g. .DS_Store) is skipped — OS/editor cruft must NOT perturb
	// the fp, else it triggers spurious whole-stack re-pushes.
	stable := mustFingerprint(t, ops, dir)
	writeAsset(t, dir, "FILES/app/.DS_Store", "junk", t0)
	if fp := mustFingerprint(t, ops, dir); fp != stable {
		t.Fatal("a dotfile changed the fp — OS cruft would trigger spurious re-pushes")
	}

	// VECTORS/ and KV/ are walked too, not just FILES/.
	preVec := mustFingerprint(t, ops, dir)
	writeAsset(t, dir, storeseed.DirVectors+"/v2.jsonl", "vec2", t0)
	if fp := mustFingerprint(t, ops, dir); fp == preVec {
		t.Fatal("new VECTORS/ asset did not change fp")
	}
	preKV := mustFingerprint(t, ops, dir)
	writeAsset(t, dir, storeseed.DirKV+"/k2.json", "kv2", t0)
	if fp := mustFingerprint(t, ops, dir); fp == preKV {
		t.Fatal("new KV/ asset did not change fp")
	}
}

// TestStackSourceFingerprintEmpty: a stack with no ops and no asset trees still
// fingerprints without error and stably — guards the os.Stat "skip missing
// tree" branch and the all-empty digest.
func TestStackSourceFingerprintEmpty(t *testing.T) {
	dir := t.TempDir()
	a := mustFingerprint(t, nil, dir)
	if b := mustFingerprint(t, nil, dir); a != b {
		t.Fatalf("empty-stack fp not stable: %s vs %s", a, b)
	}
}
