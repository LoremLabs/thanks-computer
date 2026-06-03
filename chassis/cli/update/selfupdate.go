package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	goupdate "github.com/inconshreveable/go-update"
)

// maxDownloadBytes caps each downloaded artifact (the release tarball is a
// few tens of MB; this is a generous ceiling against a runaway response).
const maxDownloadBytes = 256 << 20 // 256 MiB

// applyBinary swaps the running executable for the verified new bytes. It's
// a var so tests can redirect the write to a temp path — goupdate.Apply with
// default Options targets os.Executable(), which a test must never clobber.
var applyBinary = func(r io.Reader) error { return goupdate.Apply(r, goupdate.Options{}) }

// SelfUpdate downloads the latest release asset for this OS/arch, verifies
// its SHA256 against the release's checksums.txt, extracts the binary, and
// atomically replaces the running executable (go-update rolls back on a
// failed swap). Returns the version it updated to (no leading v).
//
// Callers must gate on the install-method policy (CanSelfUpdate) before
// calling — SelfUpdate itself does not re-check it.
func SelfUpdate(ctx context.Context, userAgent string) (string, error) {
	rel, err := LatestRelease(ctx, userAgent)
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(rel.TagName, "v")

	// Asset name embeds the version (see .github/workflows/release.yml):
	// txco_<version>_<goos>_<goarch>.tar.gz, archived alongside checksums.txt.
	assetName := fmt.Sprintf("txco_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	assetURL := rel.AssetURL(assetName)
	if assetURL == "" {
		return "", fmt.Errorf("release %s has no asset %q for %s/%s", rel.TagName, assetName, runtime.GOOS, runtime.GOARCH)
	}
	sumsURL := rel.AssetURL("checksums.txt")
	if sumsURL == "" {
		return "", fmt.Errorf("release %s has no checksums.txt", rel.TagName)
	}

	hc := &http.Client{Timeout: 60 * time.Second}

	sums, err := download(ctx, hc, userAgent, sumsURL)
	if err != nil {
		return "", fmt.Errorf("download checksums: %w", err)
	}
	wantSum, ok := checksumFor(string(sums), assetName)
	if !ok {
		return "", fmt.Errorf("checksums.txt has no entry for %s", assetName)
	}

	tarball, err := download(ctx, hc, userAgent, assetURL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", assetName, err)
	}
	sum := sha256.Sum256(tarball)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantSum) {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", assetName, got, wantSum)
	}

	bin, err := extractBinary(tarball)
	if err != nil {
		return "", err
	}

	// go-update writes the new bytes to a temp file beside the target and
	// renames it over the running binary, restoring the old one if the swap
	// fails. Tarball integrity is already verified above, so we don't pass a
	// per-binary Checksum here.
	if err := applyBinary(bytes.NewReader(bin)); err != nil {
		return "", wrapApplyErr(err)
	}
	return version, nil
}

// download GETs url and returns the full body (bounded). userAgent carries
// the CLI version.
func download(ctx context.Context, hc *http.Client, userAgent, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// checksumFor finds the sha256 hex for name in `sha256sum`-format content
// ("<hex>  <filename>" per line). strings.Fields collapses the two-space
// separator.
func checksumFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0], true
		}
	}
	return "", false
}

// extractBinary returns the bytes of the `txco` file inside the gzipped tar
// (the archive holds txco + README.md + LICENSE).
func extractBinary(targz []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "txco" {
			return io.ReadAll(io.LimitReader(tr, maxDownloadBytes))
		}
	}
	return nil, fmt.Errorf("tarball has no txco binary")
}

// wrapApplyErr turns a permission failure (binary in a system/root-owned
// dir) into actionable guidance.
func wrapApplyErr(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		exe, _ := os.Executable()
		return fmt.Errorf("cannot replace %s: permission denied — reinstall from the release page or via your package manager (%v)", exe, err)
	}
	return err
}
