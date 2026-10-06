package cli

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchABIBuild: a rebuild re-applies once it settles; a build caught
// mid-rewrite (no manifest) never fires; the build at startup counts as
// seen; a build that appears after startup fires.
func TestWatchABIBuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "txco-web")
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	var fired atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchABIBuild(ctx, dir, 20*time.Millisecond, func() { fired.Add(1) })

	wait := func(want int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if fired.Load() == want {
				time.Sleep(100 * time.Millisecond) // and it stays there
				if fired.Load() == want {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("fired %d times, want %d", fired.Load(), want)
	}

	// Not built at startup: nothing fires until a manifest appears.
	write("public/index.html", "v1")
	time.Sleep(100 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatal("fired before the build had a manifest")
	}
	write("txco-web.json", `{"abi":1}`)
	wait(1)

	// A wipe-and-rewrite: the manifest goes first, so the half-written
	// build never fires; the finished one fires once.
	must(t, os.RemoveAll(dir))
	write("public/index.html", "v2 longer")
	time.Sleep(100 * time.Millisecond)
	write("txco-web.json", `{"abi":1}`)
	wait(2)
}

// TestStackSourceFingerprintTracksTheBuild: the draft watcher's fingerprint
// moves when a bound build changes, so a rebuild re-pushes the draft.
func TestStackSourceFingerprintTracksTheBuild(t *testing.T) {
	root := abiWorkspace(t, nil)
	abi := filepath.Join(root, "www", "txco-web")
	stackDir := filepath.Join(root, "OPS", "web")
	before, err := stackSourceFingerprint(nil, stackDir, abi)
	must(t, err)
	time.Sleep(10 * time.Millisecond)
	must(t, os.WriteFile(filepath.Join(abi, "public", "index.html"), []byte("changed and longer"), 0o644))
	after, err := stackSourceFingerprint(nil, stackDir, abi)
	must(t, err)
	if before == after {
		t.Fatal("fingerprint ignored the build")
	}
	if noABI, _ := stackSourceFingerprint(nil, stackDir, ""); noABI == after {
		t.Fatal("fingerprint without a build equals one with it")
	}
}
