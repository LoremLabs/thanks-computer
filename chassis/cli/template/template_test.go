package template

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarEntry is a small fixture record for buildTarball.
type tarEntry struct {
	Name string
	Body string
	Mode int64
	Typ  byte // tar.TypeReg, tar.TypeDir, tar.TypeSymlink, ...
}

// buildTarball synthesizes a tar.gz where every entry is prefixed with
// `<repo>-<sha>/` — the same convention codeload.github.com uses, so the
// extractor's prefix-stripping logic is exercised faithfully.
func buildTarball(t *testing.T, prefix string, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Top-level directory entry mimicking codeload's output.
	if err := tw.WriteHeader(&tar.Header{
		Name:     prefix + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now(),
	}); err != nil {
		t.Fatalf("write top dir: %v", err)
	}
	for _, e := range entries {
		typ := e.Typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     prefix + "/" + e.Name,
			Mode:     mode,
			Typeflag: typ,
			Size:     int64(len(e.Body)),
			ModTime:  time.Now(),
		}
		if typ == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.Name, err)
		}
		if typ == tar.TypeReg && e.Body != "" {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatalf("write body %s: %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestParseGitHubSpec(t *testing.T) {
	cases := []struct {
		spec        string
		wantErr     bool
		wantOwner   string
		wantRepo    string
		wantRef     string
		wantSubpath string
	}{
		{spec: "github:loremlabs/templates", wantOwner: "loremlabs", wantRepo: "templates"},
		{spec: "github:loremlabs/templates/support-basic", wantOwner: "loremlabs", wantRepo: "templates", wantSubpath: "support-basic"},
		{spec: "github:loremlabs/templates/sub/deeper", wantOwner: "loremlabs", wantRepo: "templates", wantSubpath: "sub/deeper"},
		{spec: "github:loremlabs/templates@v1.2.3", wantOwner: "loremlabs", wantRepo: "templates", wantRef: "v1.2.3"},
		{spec: "github:loremlabs/templates@feature/x/sub", wantOwner: "loremlabs", wantRepo: "templates", wantRef: "feature", wantSubpath: "x/sub"},
		{spec: "github:loremlabs/templates@v1/sub", wantOwner: "loremlabs", wantRepo: "templates", wantRef: "v1", wantSubpath: "sub"},
		{spec: "github:loremlabs/templates/", wantOwner: "loremlabs", wantRepo: "templates"},
		{spec: "github:", wantErr: true},
		{spec: "github:owner", wantErr: true},
		{spec: "github:/repo", wantErr: true},
		{spec: "github:owner/repo/../escape", wantErr: true},
		{spec: "github:owner/repo@", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s, err := parseGitHub(strings.TrimPrefix(tc.spec, "github:"))
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseGitHub(%q): expected error, got %+v", tc.spec, s)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGitHub(%q): %v", tc.spec, err)
			}
			if s.owner != tc.wantOwner || s.repo != tc.wantRepo ||
				s.ref != tc.wantRef || s.subpath != tc.wantSubpath {
				t.Errorf("parseGitHub(%q) = {owner=%s repo=%s ref=%s subpath=%s}, want {owner=%s repo=%s ref=%s subpath=%s}",
					tc.spec, s.owner, s.repo, s.ref, s.subpath,
					tc.wantOwner, tc.wantRepo, tc.wantRef, tc.wantSubpath)
			}
		})
	}
}

func TestParseUnknownScheme(t *testing.T) {
	if _, err := Parse("gitlab:owner/repo"); err == nil {
		t.Error("expected error for unsupported scheme")
	}
	if _, err := Parse(""); err == nil {
		t.Error("expected error for empty spec")
	}
}

func TestFetchSubpath(t *testing.T) {
	tarball := buildTarball(t, "templates-abc123", []tarEntry{
		{Name: "README.md", Body: "repo-level"},
		{Name: "support-basic/", Typ: tar.TypeDir},
		{Name: "support-basic/100/resonator.txcl", Body: `EXEC "http://stub/100"`},
		{Name: "support-basic/200/resonator.txcl", Body: `EXEC "http://stub/200"`},
		{Name: "support-basic/triage/100/resonator.txcl", Body: `EXEC "http://triage/100"`},
		{Name: "other-template/100/resonator.txcl", Body: `EXEC "http://other/100"`},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/main") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "github:loremlabs/templates/support-basic", dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 3 {
		t.Errorf("got %d files, want 3", n)
	}

	want := map[string]string{
		"100/resonator.txcl":           `EXEC "http://stub/100"`,
		"200/resonator.txcl":           `EXEC "http://stub/200"`,
		"triage/100/resonator.txcl":    `EXEC "http://triage/100"`,
	}
	for rel, body := range want {
		got, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			t.Errorf("missing file %s: %v", rel, err)
			continue
		}
		if string(got) != body {
			t.Errorf("file %s: got %q, want %q", rel, got, body)
		}
	}

	// Files outside the subpath must NOT be copied.
	for _, stray := range []string{"README.md", "other-template/100/resonator.txcl"} {
		if _, err := os.Stat(filepath.Join(dest, stray)); err == nil {
			t.Errorf("stray file %s leaked into dest", stray)
		}
	}
}

func TestFetchWholeRepo(t *testing.T) {
	tarball := buildTarball(t, "tiny-deadbeef", []tarEntry{
		{Name: "100/resonator.txcl", Body: "rule"},
		{Name: "README.md", Body: "doc"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "github:owner/tiny", dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 2 {
		t.Errorf("got %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(dest, "100", "resonator.txcl")); err != nil {
		t.Errorf("missing 100/resonator.txcl: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("missing README.md: %v", err)
	}
}

func TestFetchExplicitRef(t *testing.T) {
	tarball := buildTarball(t, "tmpl-tag", []tarEntry{
		{Name: "100/resonator.txcl", Body: "v2"},
	})
	gotRef := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /<owner>/<repo>/tar.gz/<ref>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 4 {
			gotRef = parts[3]
		}
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	if _, err := Fetch(context.Background(), "github:owner/repo@v1.2.3", t.TempDir()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotRef != "v1.2.3" {
		t.Errorf("server saw ref %q, want v1.2.3", gotRef)
	}
}

func TestFetchDefaultBranchFallback(t *testing.T) {
	tarball := buildTarball(t, "fallback-master", []tarEntry{
		{Name: "100/resonator.txcl", Body: "ok"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject /main, accept /master.
		if strings.HasSuffix(r.URL.Path, "/main") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/master") {
			_, _ = w.Write(tarball)
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	n, err := Fetch(context.Background(), "github:owner/repo", t.TempDir())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d files, want 1", n)
	}
}

func TestFetch404Surfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	_, err := Fetch(context.Background(), "github:owner/repo@notabranch", t.TempDir())
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err missing 404: %v", err)
	}
}

func TestFetchMissingSubpath(t *testing.T) {
	tarball := buildTarball(t, "tmpl-x", []tarEntry{
		{Name: "100/resonator.txcl", Body: "ok"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "github:owner/repo/missing-template", dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d files, want 0 (subpath missing)", n)
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	tarball := buildTarball(t, "evil-deadbeef", []tarEntry{
		{Name: "../escape.txt", Body: "should not land outside dest"},
		{Name: "100/resonator.txcl", Body: "ok"},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	parent := filepath.Dir(dest)
	if _, err := Fetch(context.Background(), "github:owner/repo", dest); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Error("zip-slip: ../escape.txt landed outside dest")
	}
	// The legitimate file still made it.
	if _, err := os.Stat(filepath.Join(dest, "100", "resonator.txcl")); err != nil {
		t.Errorf("legitimate file missing: %v", err)
	}
}

// TestExtractIgnoresPaxGlobalHeader regression-locks a real-world bug:
// codeload tarballs lead with a `pax_global_header` entry. Earlier the
// extractor used the FIRST entry to derive the `<repo>-<sha>/` prefix and
// latched onto "pax_global_header" — every real entry then failed the
// prefix check and got silently skipped, returning n=0 with no error.
func TestExtractIgnoresPaxGlobalHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// Lead with a pax global header entry, mirroring codeload.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "pax_global_header",
		Typeflag: tar.TypeXGlobalHeader,
		Size:     0,
	}); err != nil {
		t.Fatalf("write pax: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "MyRepo-abcdef/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
	}); err != nil {
		t.Fatalf("write dir: %v", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name:     "MyRepo-abcdef/100/resonator.txcl",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(`EXEC "ok"`)),
	}); err != nil {
		t.Fatalf("write file hdr: %v", err)
	}
	if _, err := tw.Write([]byte(`EXEC "ok"`)); err != nil {
		t.Fatalf("write file body: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "github:owner/MyRepo", dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 1 {
		t.Fatalf("got %d files, want 1 (pax header should not eat the repo prefix)", n)
	}
	body, err := os.ReadFile(filepath.Join(dest, "100", "resonator.txcl"))
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(body) != `EXEC "ok"` {
		t.Errorf("body = %q, want EXEC \"ok\"", body)
	}
}

func TestExtractSkipsSymlinks(t *testing.T) {
	tarball := buildTarball(t, "sym-test", []tarEntry{
		{Name: "100/resonator.txcl", Body: "real"},
		{Name: "evil", Typ: tar.TypeSymlink, Body: ""},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)
	prev := SetCodeloadBaseURL(srv.URL)
	t.Cleanup(func() { SetCodeloadBaseURL(prev) })

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "github:owner/repo", dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d files, want 1 (symlink skipped)", n)
	}
}
