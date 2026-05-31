package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// absFixture returns the absolute path to the support-basic example package,
// skipping the test if it's absent.
func absFixture(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "examples", "packages", "support-basic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, "txco.package.yaml")); err != nil {
		t.Skipf("example package not found: %v", err)
	}
	return p
}

// mustInstall installs the fixture (as a dir: source) into the current
// workspace as <stack>, failing the test on a non-zero exit.
func mustInstall(t *testing.T, fixture, stack string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + fixture, "--as", stack}, &out, &errb); code != 0 {
		t.Fatalf("install: %s", errb.String())
	}
}

// TestInstalledStackHashReproducesLockfile is the load-bearing test: the hash
// recomputed from the on-disk stack must equal what install stored in the
// lockfile, proving stackEditState uses the install-time pipeline (and NOT the
// chassis-state loadLocalStackFiles hash, which would never match).
func TestInstalledStackHashReproducesLockfile(t *testing.T) {
	fixture := absFixture(t)
	ws := t.TempDir()
	t.Chdir(ws)
	mustInstall(t, fixture, "support")

	lf, err := lockfile.Read(ws)
	if err != nil || len(lf.Packages) != 1 {
		t.Fatalf("lockfile: err=%v packages=%+v", err, lf.Packages)
	}
	stored := lf.Packages[0].ManifestHash
	if stored == "" {
		t.Fatal("install stored no ManifestHash")
	}

	got, exists, err := installedStackHash(ws, "support")
	if err != nil || !exists {
		t.Fatalf("installedStackHash: err=%v exists=%v", err, exists)
	}
	if got != stored {
		t.Fatalf("installedStackHash=%q != lockfile ManifestHash=%q — wrong hash pipeline", got, stored)
	}
	if st, _ := stackEditState(ws, lf.Packages[0]); st != "clean" {
		t.Errorf("fresh install: state=%q, want clean", st)
	}

	// Editing a .txcl flips to "edited".
	txcl := filepath.Join(ws, "OPS", "support", "0200_NOTIFY", "notify.txcl")
	b, err := os.ReadFile(txcl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(txcl, append(b, []byte("\n# local edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := stackEditState(ws, lf.Packages[0]); st != "edited" {
		t.Errorf("after edit: state=%q, want edited", st)
	}

	// Editing only the colocated .js does NOT flip "edited" (documented blind
	// spot — only .txcl + normalized mocks are hashed). Restore the .txcl first.
	if err := os.WriteFile(txcl, b, 0o644); err != nil {
		t.Fatal(err)
	}
	js := filepath.Join(ws, "OPS", "support", "0100_TRIAGE", "classify.js")
	if jb, err := os.ReadFile(js); err == nil {
		_ = os.WriteFile(js, append(jb, []byte("\n// edit\n")...), 0o644)
		if st, _ := stackEditState(ws, lf.Packages[0]); st != "clean" {
			t.Errorf("after .js-only edit: state=%q, want clean (blind spot)", st)
		}
	}

	// Removing the stack dir → "missing".
	if err := os.RemoveAll(filepath.Join(ws, "OPS", "support")); err != nil {
		t.Fatal(err)
	}
	if st, _ := stackEditState(ws, lf.Packages[0]); st != "missing" {
		t.Errorf("after rm: state=%q, want missing", st)
	}
}

// TestEditGuardRefusesClobber covers all three guarded paths: reinstall and
// upgrade over a hand-edited stack must refuse without --force and leave the
// edits intact; --force re-materializes the package content.
func TestEditGuardRefusesClobber(t *testing.T) {
	fixture := absFixture(t)
	ws := t.TempDir()
	t.Chdir(ws)
	mustInstall(t, fixture, "support")

	txcl := filepath.Join(ws, "OPS", "support", "0200_NOTIFY", "notify.txcl")
	orig, _ := os.ReadFile(txcl)
	edited := append(append([]byte{}, orig...), []byte("\n# hand edit\n")...)
	if err := os.WriteFile(txcl, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	// Reinstall (same ref) refuses without --force.
	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + fixture, "--as", "support"}, &out, &errb); code == 0 {
		t.Errorf("reinstall over edits should fail; out=%s", out.String())
	}
	if !strings.Contains(errb.String(), "local edits") || !strings.Contains(errb.String(), "txco diff") {
		t.Errorf("reinstall error should mention local edits + txco diff: %q", errb.String())
	}
	if b, _ := os.ReadFile(txcl); !bytes.Equal(b, edited) {
		t.Error("a refused reinstall must not modify the file")
	}

	// Upgrade refuses too.
	out.Reset()
	errb.Reset()
	if code := runUpgrade([]string{"support"}, &out, &errb); code == 0 {
		t.Errorf("upgrade over edits should fail; out=%s", out.String())
	}
	if !strings.Contains(errb.String(), "local edits") {
		t.Errorf("upgrade error should mention local edits: %q", errb.String())
	}
	if b, _ := os.ReadFile(txcl); !bytes.Equal(b, edited) {
		t.Error("a refused upgrade must not modify the file")
	}

	// --force reinstall re-materializes the package, discarding the edit.
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"dir:" + fixture, "--as", "support", "--force"}, &out, &errb); code != 0 {
		t.Fatalf("forced reinstall: %s", errb.String())
	}
	if b, _ := os.ReadFile(txcl); !bytes.Equal(b, orig) {
		t.Errorf("forced reinstall should restore package content; got %q", string(b))
	}
}

// TestRemove covers delete-files + entry, --keep-files, and the edit guard.
func TestRemove(t *testing.T) {
	fixture := absFixture(t)
	ws := t.TempDir()
	t.Chdir(ws)
	mustInstall(t, fixture, "support")

	// --keep-files drops the entry but leaves OPS/support/.
	var out, errb bytes.Buffer
	if code := runRemove([]string{"support", "--keep-files"}, &out, &errb); code != 0 {
		t.Fatalf("remove --keep-files: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); err != nil {
		t.Error("--keep-files should leave OPS/support/ in place")
	}
	if lf, _ := lockfile.Read(ws); lf.FindStack("support") != nil {
		t.Error("--keep-files should still drop the lockfile entry")
	}
	// The kept files are now untracked; clear them so the next install starts clean.
	if err := os.RemoveAll(filepath.Join(ws, "OPS", "support")); err != nil {
		t.Fatal(err)
	}

	// Reinstall, then a full remove deletes files + entry.
	mustInstall(t, fixture, "support")
	out.Reset()
	errb.Reset()
	if code := runRemove([]string{"support"}, &out, &errb); code != 0 {
		t.Fatalf("remove: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); !os.IsNotExist(err) {
		t.Error("remove should delete OPS/support/")
	}
	if lf, _ := lockfile.Read(ws); lf.FindStack("support") != nil {
		t.Error("remove should drop the lockfile entry")
	}

	// An edited stack refuses removal without --force.
	mustInstall(t, fixture, "support")
	txcl := filepath.Join(ws, "OPS", "support", "0200_NOTIFY", "notify.txcl")
	b, _ := os.ReadFile(txcl)
	_ = os.WriteFile(txcl, append(b, []byte("\n# edit\n")...), 0o644)
	out.Reset()
	errb.Reset()
	if code := runRemove([]string{"support"}, &out, &errb); code == 0 {
		t.Error("remove of an edited stack should refuse without --force")
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); err != nil {
		t.Error("a refused remove must not delete files")
	}
	// --keep-files removes the entry even when edited (files are safe anyway).
	out.Reset()
	errb.Reset()
	if code := runRemove([]string{"support", "--keep-files"}, &out, &errb); code != 0 {
		t.Errorf("remove --keep-files of an edited stack should succeed: %s", errb.String())
	}
}

// TestListJSON asserts the --json shape for an as-stack + a vendor-only entry.
func TestListJSON(t *testing.T) {
	fixture := absFixture(t)
	ws := t.TempDir()
	t.Chdir(ws)
	mustInstall(t, fixture, "support")

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + fixture, "--vendor-only"}, &out, &errb); code != 0 {
		t.Fatalf("vendor install: %s", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := runList([]string{"--json"}, &out, &errb); code != 0 {
		t.Fatalf("list --json: %s", errb.String())
	}
	var rows []struct {
		Name        string `json:"name"`
		Mode        string `json:"mode"`
		InstalledAs string `json:"installedAs"`
		Edited      *bool  `json:"edited"`
		Present     bool   `json:"present"`
		DigestShort string `json:"digestShort"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (as-stack + vendor-only), got %d: %s", len(rows), out.String())
	}
	var sawStack, sawVendor bool
	for _, r := range rows {
		switch r.Mode {
		case "as-stack":
			sawStack = true
			if r.Edited == nil {
				t.Error("as-stack row should have a non-null edited flag")
			} else if *r.Edited {
				t.Error("fresh as-stack should be edited=false")
			}
			if r.DigestShort != "" {
				t.Errorf("dir: install should have no digest, got %q", r.DigestShort)
			}
		case "vendor-only":
			sawVendor = true
			if r.Edited != nil {
				t.Error("vendor-only row should have null edited flag")
			}
		}
	}
	if !sawStack || !sawVendor {
		t.Errorf("missing a row type: stack=%v vendor=%v", sawStack, sawVendor)
	}
}

// TestUpgradeOCIRoundTrip publishes 0.1.0 to an in-process OCI store, installs
// via a moving tag, re-publishes changed content to the same tag, then upgrades
// — asserting the lockfile re-pins to the new digest + version and OPS/ is
// re-materialized. No registry, no network.
func TestUpgradeOCIRoundTrip(t *testing.T) {
	fixture := absFixture(t)

	// A mutable copy of the fixture we can re-publish with changed content.
	pkg := t.TempDir()
	if _, err := copyTree(fixture, pkg); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevPush := source.SetPushRepositoryFactory(func(string) (oras.Target, error) { return store, nil })
	prevPull := source.SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { source.SetPushRepositoryFactory(prevPush); source.SetRepositoryFactory(prevPull) })

	const ref = "oci://registry.thanks.computer/txco/support-basic:latest"

	publish := func() {
		t.Helper()
		var out, errb bytes.Buffer
		if code := runPackage([]string{"publish", "--to", ref, pkg}, &out, &errb); code != 0 {
			t.Fatalf("publish: %s", errb.String())
		}
	}
	publish()

	ws := t.TempDir()
	t.Chdir(ws)
	var out, errb bytes.Buffer
	if code := runInstall([]string{ref, "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install: %s", errb.String())
	}
	lf, _ := lockfile.Read(ws)
	before := lf.Packages[0]
	if !strings.Contains(before.Resolved, "@sha256:") {
		t.Fatalf("install should pin a digest, got %q", before.Resolved)
	}

	// Change a rule + bump the version, then re-publish to the SAME moving tag.
	notify := filepath.Join(pkg, "OPS", "support", "0200_NOTIFY", "notify.txcl")
	nb, _ := os.ReadFile(notify)
	if err := os.WriteFile(notify, append(nb, []byte("\nEMIT .v = 2\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpVersion(t, filepath.Join(pkg, "txco.package.yaml"), "0.1.0", "0.2.0")
	publish()

	// Upgrade picks up the new digest + version and re-materializes OPS/.
	out.Reset()
	errb.Reset()
	if code := runUpgrade([]string{"support"}, &out, &errb); code != 0 {
		t.Fatalf("upgrade: %s\n%s", errb.String(), out.String())
	}
	lf, _ = lockfile.Read(ws)
	after := lf.Packages[0]
	if after.Resolved == before.Resolved {
		t.Errorf("upgrade should re-pin the digest; still %q", after.Resolved)
	}
	if after.Version != "0.2.0" {
		t.Errorf("upgrade should record the new version, got %q", after.Version)
	}
	if after.ManifestHash == before.ManifestHash {
		t.Error("upgrade should record a new manifest hash")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "OPS", "support", "0200_NOTIFY", "notify.txcl")); !strings.Contains(string(b), "EMIT .v = 2") {
		t.Error("upgrade should re-materialize the new rule content")
	}

	// A second upgrade with no change reports up-to-date and rewrites nothing.
	out.Reset()
	errb.Reset()
	if code := runUpgrade([]string{"support"}, &out, &errb); code != 0 {
		t.Fatalf("idempotent upgrade: %s", errb.String())
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Errorf("second upgrade should be a no-op, got %q", out.String())
	}
}

// TestUpgradeAllContinuesOnFailure: `upgrade --all` keeps going past a failing
// target, reports it, and exits non-zero while leaving good entries intact.
func TestUpgradeAllContinuesOnFailure(t *testing.T) {
	fixture := absFixture(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := copyTree(fixture, dirA); err != nil {
		t.Fatal(err)
	}
	if _, err := copyTree(fixture, dirB); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	t.Chdir(ws)
	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + dirA, "--as", "a"}, &out, &errb); code != 0 {
		t.Fatalf("install a: %s", errb.String())
	}
	if code := runInstall([]string{"dir:" + dirB, "--as", "b"}, &out, &errb); code != 0 {
		t.Fatalf("install b: %s", errb.String())
	}

	// Break b's source so its re-resolve fails.
	if err := os.RemoveAll(dirB); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	code := runUpgrade([]string{"--all"}, &out, &errb)
	if code == 0 {
		t.Errorf("upgrade --all with a failing target should exit non-zero")
	}
	if !strings.Contains(out.String(), "1 failed") {
		t.Errorf("summary should report 1 failed: %q", out.String())
	}
	// Both entries persist — a failure doesn't drop the lockfile entry.
	lf, _ := lockfile.Read(ws)
	if lf.FindStack("a") == nil || lf.FindStack("b") == nil {
		t.Errorf("both entries should persist after a partial failure: %+v", lf.Packages)
	}
}

// bumpVersion rewrites `version: <old>` to <new> in a package manifest.
func bumpVersion(t *testing.T, manifestPath, old, neu string) {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "version: "+old, "version: "+neu, 1)
	if s == string(b) {
		t.Fatalf("version: %s not found in %s", old, manifestPath)
	}
	if err := os.WriteFile(manifestPath, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
