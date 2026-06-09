package javybin

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// reset clears the process-memoized resolution so each test starts clean.
func reset() { mu.Lock(); resolved = ""; mu.Unlock() }

// TestAssetStemCurrentHost checks the GOOS/GOARCH → release-asset mapping for
// the host. Supported hosts must yield a "javy-…-v<Version>" stem; anything
// else must error (so download() can surface a clean "unsupported platform").
func TestAssetStemCurrentHost(t *testing.T) {
	stem, err := assetStem()
	supported := (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") &&
		(runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows")
	if !supported {
		if err == nil {
			t.Fatalf("expected error for unsupported %s/%s, got stem %q", runtime.GOOS, runtime.GOARCH, stem)
		}
		return
	}
	if err != nil {
		t.Fatalf("assetStem(%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
	}
	if !strings.HasPrefix(stem, "javy-") || !strings.HasSuffix(stem, "-v"+Version) {
		t.Errorf("stem %q malformed (want javy-<arch>-<os>-v%s)", stem, Version)
	}
}

// TestResolveNoDownloadUnavailable: with no javy on PATH, no cached binary, no
// override, and downloads forbidden, Resolve must report ErrUnavailable rather
// than touching the network — the contract publish relies on to ship
// source-only offline.
func TestResolveNoDownloadUnavailable(t *testing.T) {
	reset()
	t.Cleanup(reset)
	t.Setenv("PATH", t.TempDir())      // no javy discoverable
	t.Setenv("TXCO_HOME", t.TempDir()) // no cached managed binary
	t.Setenv("TXCO_JAVY", "")          // no explicit override
	t.Setenv("TXCO_JAVY_NO_DOWNLOAD", "1")

	if _, err := Resolve(context.Background(), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

// TestResolveBadOverride: a TXCO_JAVY pointing at a non-runnable path is an
// ErrUnavailable, never a silent fallthrough to PATH/download.
func TestResolveBadOverride(t *testing.T) {
	reset()
	t.Cleanup(reset)
	t.Setenv("TXCO_JAVY", filepath.Join(t.TempDir(), "does-not-exist"))

	if _, err := Resolve(context.Background(), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable for bad override, got %v", err)
	}
}
