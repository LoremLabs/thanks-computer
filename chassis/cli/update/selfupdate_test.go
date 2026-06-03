package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

func TestChecksumFor(t *testing.T) {
	sums := "abc123  txco_0.3.0_darwin_arm64.tar.gz\ndef456  checksums.txt\n"
	got, ok := checksumFor(sums, "txco_0.3.0_darwin_arm64.tar.gz")
	if !ok || got != "abc123" {
		t.Errorf("checksumFor = %q, %v; want abc123, true", got, ok)
	}
	if _, ok := checksumFor(sums, "missing.tar.gz"); ok {
		t.Errorf("expected miss for absent file")
	}
}

// makeTarGz builds a release-shaped archive (txco + README + LICENSE).
func makeTarGz(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"README.md", []byte("readme")},
		{"txco", bin},
		{"LICENSE", []byte("license")},
	} {
		hdr := &tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	got, err := extractBinary(makeTarGz(t, []byte("BINARY")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "BINARY" {
		t.Errorf("extractBinary = %q, want BINARY", got)
	}
}

// releaseServer stands up a fake GitHub release + asset host and points
// githubAPIBase at it. The checksums line uses wantSum (pass the real digest
// for the happy path, a bogus one to exercise the mismatch guard).
func releaseServer(t *testing.T, version string, tgz []byte, wantSum string) {
	t.Helper()
	asset := fmt.Sprintf("txco_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/repos/loremlabs/thanks-computer/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Release{
			TagName: "v" + version,
			Assets: []Asset{
				{Name: asset, DownloadURL: base + "/dl/" + asset},
				{Name: "checksums.txt", DownloadURL: base + "/dl/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", wantSum, asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base = srv.URL
	old := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = old })
}

// stubApply redirects the binary swap to a buffer (never the test runner).
func stubApply(t *testing.T) *bytes.Buffer {
	t.Helper()
	var got bytes.Buffer
	old := applyBinary
	applyBinary = func(r io.Reader) error { _, err := io.Copy(&got, r); return err }
	t.Cleanup(func() { applyBinary = old })
	return &got
}

func TestSelfUpdate(t *testing.T) {
	const version = "9.9.9"
	bin := []byte("NEW-TXCO-BINARY")
	tgz := makeTarGz(t, bin)
	sum := sha256.Sum256(tgz)
	releaseServer(t, version, tgz, hex.EncodeToString(sum[:]))
	applied := stubApply(t)

	got, err := SelfUpdate(context.Background(), "txco-cli/test")
	if err != nil {
		t.Fatalf("SelfUpdate: %v", err)
	}
	if got != version {
		t.Errorf("version = %q, want %q", got, version)
	}
	if applied.String() != string(bin) {
		t.Errorf("applied %q, want %q", applied.String(), bin)
	}
}

func TestSelfUpdateChecksumMismatch(t *testing.T) {
	const version = "9.9.9"
	tgz := makeTarGz(t, []byte("payload"))
	releaseServer(t, version, tgz, "deadbeef") // wrong checksum

	applyCalled := false
	old := applyBinary
	applyBinary = func(io.Reader) error { applyCalled = true; return nil }
	t.Cleanup(func() { applyBinary = old })

	if _, err := SelfUpdate(context.Background(), "txco-cli/test"); err == nil {
		t.Fatal("expected checksum-mismatch error")
	}
	if applyCalled {
		t.Error("applyBinary must not run when the checksum fails")
	}
}
