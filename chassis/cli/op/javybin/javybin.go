// Package javybin resolves a `javy` CLI binary matching the Javy version the
// vendored QuickJS plugin was emitted from (chassis/compute/javyplugin). The
// op build path (chassis/cli/op) needs javy to compile JS/TS nano-ops to wasm,
// but we don't want users to install it by hand — so Resolve transparently
// downloads the pinned release into the txco home on first use and caches it.
//
// Resolution order (first hit wins):
//
//  1. $TXCO_JAVY — an explicit binary path the operator chose; trusted as-is.
//  2. A `javy` already on PATH whose `--version` matches the pin — respected so
//     we never download when a correct toolchain is already present.
//  3. A previously downloaded managed binary under <txco-home>/tools/.
//  4. A freshly downloaded release, checksum-verified against its .sha256.
//
// The managed binary is version-stamped in its filename, so a Javy bump (new
// pin) lands a new file rather than silently reusing an incompatible one.
package javybin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/compute/javyplugin"
)

// Version is the Javy toolchain Resolve targets — pinned to the vendored
// plugin so built modules' bytecode always matches what the runtime links.
const Version = javyplugin.JavyVersion

// releaseBase is the GitHub release download root for javy assets.
const releaseBase = "https://github.com/bytecodealliance/javy/releases/download"

// ErrUnavailable wraps every terminal failure to obtain javy (unsupported
// platform, offline with no cached/PATH binary, checksum mismatch). Callers
// classify it distinctly from a genuine compile error — e.g. the demo server
// surfaces it as "compile_unavailable" with an install hint.
var ErrUnavailable = errors.New("javy unavailable")

// httpClient is generous: the gzipped asset is ~11 MB and the binary ~33 MB.
var httpClient = &http.Client{Timeout: 5 * time.Minute}

var (
	mu       sync.Mutex
	resolved string // process-memoized successful path
)

// Resolve returns the path to a usable javy binary at the pinned Version,
// downloading and caching it if necessary. progress (may be nil) receives
// one-time human-readable status lines for the download. Safe for concurrent
// use; a successful result is memoized for the process.
func Resolve(ctx context.Context, progress io.Writer) (string, error) {
	mu.Lock()
	defer mu.Unlock()

	if resolved != "" {
		return resolved, nil
	}

	// 1. Explicit operator override.
	if p := os.Getenv("TXCO_JAVY"); p != "" {
		if probeVersion(ctx, p) == "" {
			return "", fmt.Errorf("%w: TXCO_JAVY=%q is not a runnable javy binary", ErrUnavailable, p)
		}
		resolved = p
		return p, nil
	}

	// 2. A matching javy already on PATH — use it, skip the download. A
	//    mismatched PATH javy is deliberately ignored: its bytecode may not
	//    decode against the pinned plugin, so we prefer a correct managed copy.
	if p, err := exec.LookPath("javy"); err == nil && probeVersion(ctx, p) == Version {
		resolved = p
		return p, nil
	}

	// 3. Previously downloaded managed binary.
	dst, err := managedPath()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if st, serr := os.Stat(dst); serr == nil && st.Mode()&0o111 != 0 {
		resolved = dst
		return dst, nil
	}

	// 4. Download, verify, cache — unless auto-download is disabled (offline
	//    CI / hermetic tests / air-gapped hosts opt out via env). Callers that
	//    treat ErrUnavailable as non-fatal (e.g. publish → ship source-only)
	//    then degrade gracefully without touching the network.
	if os.Getenv("TXCO_JAVY_NO_DOWNLOAD") != "" {
		return "", fmt.Errorf("%w: auto-download disabled (TXCO_JAVY_NO_DOWNLOAD set) and no compatible javy on PATH; install it from github.com/bytecodealliance/javy/releases or set TXCO_JAVY", ErrUnavailable)
	}
	if err := download(ctx, dst, progress); err != nil {
		return "", err
	}
	resolved = dst
	return dst, nil
}

// managedPath is <txco-home>/tools/javy-<version>[.exe]. HomeDir honors
// TXCO_HOME / XDG_CONFIG_HOME / ~/.config/txco and does not create the dir.
func managedPath() (string, error) {
	home, ok := auth.HomeDir()
	if !ok {
		return "", errors.New("cannot resolve txco home directory for the javy cache")
	}
	name := "javy-" + Version
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(home, "tools", name), nil
}

// assetStem maps the host GOOS/GOARCH to javy's release asset naming, e.g.
// "javy-arm-macos-v8.1.1" (the .gz / .gz.sha256 suffixes are added by callers).
func assetStem() (string, error) {
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "arm"
	default:
		return "", fmt.Errorf("no prebuilt javy for GOARCH=%s", runtime.GOARCH)
	}
	var goos string
	switch runtime.GOOS {
	case "darwin":
		goos = "macos"
	case "linux":
		goos = "linux"
	case "windows":
		goos = "windows"
	default:
		return "", fmt.Errorf("no prebuilt javy for GOOS=%s", runtime.GOOS)
	}
	return fmt.Sprintf("javy-%s-%s-v%s", arch, goos, Version), nil
}

// download fetches the pinned gzipped release, verifies it against the
// published .sha256, decompresses it, and installs it atomically at dst.
func download(ctx context.Context, dst string, progress io.Writer) error {
	stem, err := assetStem()
	if err != nil {
		return fmt.Errorf("%w: %v; install it from github.com/bytecodealliance/javy/releases or set TXCO_JAVY", ErrUnavailable, err)
	}
	gzURL := releaseBase + "/v" + Version + "/" + stem + ".gz"

	if progress != nil {
		fmt.Fprintf(progress, "[txco] fetching javy %s toolchain (one-time, ~11 MB)...\n", Version)
	}

	// The .sha256 covers the .gz asset.
	sumTxt, err := httpGet(ctx, gzURL+".sha256")
	if err != nil {
		return fmt.Errorf("%w: fetch checksum: %v", ErrUnavailable, err)
	}
	wantSum := ""
	if f := strings.Fields(string(sumTxt)); len(f) > 0 {
		wantSum = strings.ToLower(f[0])
	}
	if len(wantSum) != 64 {
		return fmt.Errorf("%w: unexpected checksum file for %s", ErrUnavailable, stem)
	}

	gzBytes, err := httpGet(ctx, gzURL)
	if err != nil {
		return fmt.Errorf("%w: download: %v", ErrUnavailable, err)
	}
	gotSum := sha256.Sum256(gzBytes)
	if hex.EncodeToString(gotSum[:]) != wantSum {
		return fmt.Errorf("%w: checksum mismatch for %s (want %s, got %x)", ErrUnavailable, stem, wantSum, gotSum)
	}

	zr, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		return fmt.Errorf("%w: gunzip: %v", ErrUnavailable, err)
	}
	defer zr.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	// Write to a sibling temp file, then rename — atomic, and concurrent
	// resolvers racing on the same dst each install their own copy safely.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".javy-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := io.Copy(tmp, zr); err != nil {
		tmp.Close()
		return fmt.Errorf("%w: write: %v", ErrUnavailable, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("%w: install: %v", ErrUnavailable, err)
	}

	if progress != nil {
		fmt.Fprintf(progress, "[txco] javy %s ready (cached in %s)\n", Version, filepath.Dir(dst))
	}
	return nil
}

// httpGet does a context-bound GET and returns the body, erroring on non-200.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// probeVersion runs `<bin> --version` and returns the reported version
// (e.g. "8.1.1" from "javy 8.1.1"), or "" if the binary can't be run.
func probeVersion(ctx context.Context, bin string) string {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	if f := strings.Fields(string(out)); len(f) >= 2 {
		return f[1]
	}
	return ""
}
