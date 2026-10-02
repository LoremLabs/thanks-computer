package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	computeapi "github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/computesrc"
)

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

// builtWithSource fakes a compute built from source, with its bundle's local
// copy on disk.
func builtWithSource(t *testing.T, digest, srcHash string) computeapi.Built {
	t.Helper()
	f := filepath.Join(t.TempDir(), srcHash+".src.json")
	if err := os.WriteFile(f, []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return computeapi.Built{
		Digest: digest, Ref: "compute://sha256/" + digest,
		Source: &computesrc.Bundle{V: 1, Entry: "a.js"}, SourceHash: srcHash, SourceFile: f,
	}
}

// One fingerprint-only row per compute the stack's ops EXEC whose source was
// built here; a prebuilt wasm, an unreferenced compute and a repeat reference
// add nothing.
func TestComputeSourceRows(t *testing.T) {
	plan, pull := hex64('a'), hex64('b')
	prebuilt, elsewhere := hex64('c'), hex64('d')
	built := []computeapi.Built{
		builtWithSource(t, plan, hex64('1')),
		builtWithSource(t, pull, hex64('2')),
		builtWithSource(t, elsewhere, hex64('3')),
		{Digest: prebuilt, Ref: "compute://sha256/" + prebuilt},
	}
	ops := []bundle.Op{
		{Stack: "site", Scope: 100, Name: "plan", Txcl: `EXEC "compute://sha256/` + plan + `"`},
		{Stack: "site", Scope: 200, Name: "pull", Txcl: `EXEC "compute://sha256/` + pull + `"
WITH x = 1`},
		{Stack: "site", Scope: 300, Name: "again", Txcl: `EXEC "compute://sha256/` + plan + `"`},
		{Stack: "site", Scope: 400, Name: "pkg", Txcl: `EXEC "compute://sha256/` + prebuilt + `"`},
	}
	rows, uploads := computeSourceRows(ops, built)
	if len(rows) != 2 || len(uploads) != 2 {
		t.Fatalf("rows=%v uploads=%v", rows, uploads)
	}
	if rows[0].Path != computesrc.Path(plan) || rows[0].ContentHash != hex64('1') || rows[0].Encoding != "cas" || rows[0].Content != "" {
		t.Fatalf("row 0 = %+v", rows[0])
	}
	if rows[1].Path != computesrc.Path(pull) {
		t.Fatalf("row 1 = %+v", rows[1])
	}
	for _, u := range uploads {
		if u.Size == 0 || u.LocalPath == "" {
			t.Fatalf("upload without its local bytes: %+v", u)
		}
	}
}

// fakeBlobChassis serves the blob plane: POST /blobs/missing (unless old),
// HEAD and PUT by hash. It records the PUTs.
type fakeBlobChassis struct {
	mu    sync.Mutex
	have  map[string]bool
	puts  []string
	heads int
	old   bool
}

func (f *fakeBlobChassis) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/blobs/missing"):
		if f.old {
			http.NotFound(w, r)
			return
		}
		var req struct{ Hashes []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		missing := []string{}
		for _, h := range req.Hashes {
			if !f.have[h] {
				missing = append(missing, h)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string][]string{"missing": missing})
	case r.Method == http.MethodHead:
		f.heads++
		if f.have[filepath.Base(r.URL.Path)] {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPut:
		h := filepath.Base(r.URL.Path)
		_, _ = io.Copy(io.Discard, r.Body)
		f.puts = append(f.puts, h)
		f.have[h] = true
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func casUploadOf(t *testing.T, hash string) casUpload {
	t.Helper()
	p := filepath.Join(t.TempDir(), hash)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return casUpload{Path: "COMPUTES/" + hash + ".json", LocalPath: p, Hash: hash, Size: 1}
}

// One batch probe decides what to upload; only the misses are PUT, once
// each, and no per-hash HEAD is sent.
func TestEnsureBlobsResidentBatches(t *testing.T) {
	fake := &fakeBlobChassis{have: map[string]bool{hex64('1'): true}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c := client.New(client.Target{Addr: srv.URL, Tenant: "acme"})

	ups := []casUpload{casUploadOf(t, hex64('1')), casUploadOf(t, hex64('2')), casUploadOf(t, hex64('2'))}
	var out bytes.Buffer
	if err := ensureBlobsResident(context.Background(), c, ups, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if len(fake.puts) != 1 || fake.puts[0] != hex64('2') || fake.heads != 0 {
		t.Fatalf("puts=%v heads=%d", fake.puts, fake.heads)
	}
}

// An older chassis (no batch endpoint) still works through per-hash HEADs,
// and is reported as not keeping compute source.
func TestOlderChassisFallsBack(t *testing.T) {
	fake := &fakeBlobChassis{have: map[string]bool{}, old: true}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	c := client.New(client.Target{Addr: srv.URL, Tenant: "acme"})

	var out bytes.Buffer
	if err := ensureBlobsResident(context.Background(), c, []casUpload{casUploadOf(t, hex64('3'))}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if fake.heads != 1 || len(fake.puts) != 1 {
		t.Fatalf("heads=%d puts=%v", fake.heads, fake.puts)
	}

	built := []computeapi.Built{builtWithSource(t, hex64('a'), hex64('1'))}
	var errb bytes.Buffer
	if chassisKeepsComputeSource(context.Background(), c, built, &errb, "apply") {
		t.Fatal("an older chassis reported as keeping compute source")
	}
	if !strings.Contains(errb.String(), "doesn't keep compute source") {
		t.Fatalf("notice = %q", errb.String())
	}

	fake.old = false
	if !chassisKeepsComputeSource(context.Background(), c, built, &errb, "apply") {
		t.Fatal("a current chassis reported as not keeping compute source")
	}
	if chassisKeepsComputeSource(context.Background(), c, []computeapi.Built{{Digest: hex64('c')}}, &errb, "apply") {
		t.Fatal("no source built, yet rows would be sent")
	}
}

// sourceChassis answers a push (computes, blob plane, draft → files →
// validate → activate) and records what the draft carried and which blob
// calls arrived.
type sourceChassis struct {
	mu        sync.Mutex
	files     []client.StackFile
	blobCalls int
}

func (s *sourceChassis) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.Contains(r.URL.Path, "/computes/"):
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(r.URL.Path, "/blobs/missing"):
		s.blobCalls++
		var req struct{ Hashes []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string][]string{"missing": req.Hashes})
	case strings.Contains(r.URL.Path, "/blobs/sha256/"):
		s.blobCalls++
		_, _ = io.Copy(io.Discard, r.Body)
	case strings.HasSuffix(r.URL.Path, "/draft"):
		_, _ = w.Write([]byte(`{"version_number":3}`))
	case strings.HasSuffix(r.URL.Path, "/files"):
		var req struct{ Files []client.StackFile }
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.files = req.Files
		_, _ = w.Write([]byte(`{"manifest_hash":"abc"}`))
	case strings.HasSuffix(r.URL.Path, "/validate"):
		_, _ = w.Write([]byte(`{"ok":true,"checked":1}`))
	case strings.HasSuffix(r.URL.Path, "/activate"):
		_, _ = w.Write([]byte(`{"version_number":3,"prior_version_number":2}`))
	default:
		http.NotFound(w, r)
	}
}

func (s *sourceChassis) sourceRows() int {
	n := 0
	for _, f := range s.files {
		if computesrc.IsPath(f.Path) {
			n++
		}
	}
	return n
}

// By default a push records the compute's source; --no-source sends no
// COMPUTES/ row and touches the blob plane not at all.
func TestPushNoSource(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	for p, body := range map[string]string{
		"OPS/api/100/hello.txcl": `EXEC "op://hello"`,
		"OPS/api/100/hello.js":   `import { op } from "@txco/op"; export default op(({ input }) => input);`,
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		args      []string
		rows      int
		blobCalls bool
	}{
		{nil, 1, true},
		{[]string{"--no-source"}, 0, false},
	} {
		fake := &sourceChassis{}
		srv := httptest.NewServer(fake)
		var out, errb bytes.Buffer
		args := append([]string{"api", root, "--addr", srv.URL}, tc.args...)
		if code := runPush(args, &out, &errb); code != 0 {
			srv.Close()
			t.Fatalf("%v: exit=%d stderr=%q", tc.args, code, errb.String())
		}
		srv.Close()
		if got := fake.sourceRows(); got != tc.rows {
			t.Errorf("%v: COMPUTES/ rows = %d, want %d (files %+v)", tc.args, got, tc.rows, fake.files)
		}
		if (fake.blobCalls > 0) != tc.blobCalls {
			t.Errorf("%v: blob calls = %d", tc.args, fake.blobCalls)
		}
	}
}
