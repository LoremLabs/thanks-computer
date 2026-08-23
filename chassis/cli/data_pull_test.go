package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func TestDriftHelpers(t *testing.T) {
	rows := []client.StackBlob{
		{Name: "a", SHA256: "A", Drifted: false},
		{Name: "b", SHA256: "B2", SeededSHA: "B1", Drifted: true}, // local still has B1 → conflict
		{Name: "c", SHA256: "C2", SeededSHA: "C1", Drifted: true}, // local pulled C2 → fast-forward
		{Name: "d", SHA256: "D2", SeededSHA: "D1", Drifted: true}, // local dropped it → conflict
	}
	local := map[string]string{"a": "A", "b": "B1", "c": "C2"}
	d := driftedStackBlobs(rows, local)
	if len(d) != 2 || d[0].Name != "b" || d[1].Name != "d" {
		t.Fatalf("driftedStackBlobs = %+v", d)
	}
	if h := localBlobHashes([]client.StackFile{{Path: "BLOBS/x/y.txt", ContentHash: "H"}, {Path: "KV/k.jsonl", ContentHash: "K"}}); len(h) != 1 || h["x/y.txt"] != "H" {
		t.Fatalf("localBlobHashes = %v", h)
	}
	if hasBlobRows([]client.StackFile{{Path: "KV/x.jsonl"}}) || !hasBlobRows([]client.StackFile{{Path: "BLOBS/a.txt"}}) {
		t.Fatal("hasBlobRows")
	}
}

// fakeBlobAdmin serves the two endpoints `data pull` / the `data apply`
// drift check use: the stack record, the seeded-names listing, and the
// blob GET plane. drafts counts POSTs to the draft endpoint — a refused
// apply must never reach it.
func fakeBlobAdmin(t *testing.T, rows []client.StackBlob, blobs map[string][]byte) (*httptest.Server, *int) {
	t.Helper()
	drafts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks/demo":
			v := int64(1)
			_ = json.NewEncoder(w).Encode(client.StackRecord{Name: "demo", ActiveVersion: &v})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks/demo/blobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"stack": "demo", "names": rows, "count": len(rows)})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/tenants/default/blobs/sha256/"):
			h := strings.TrimPrefix(r.URL.Path, "/v1/tenants/default/blobs/sha256/")
			b, ok := blobs[h]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
		case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/v1/tenants/default/blobs/sha256/"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/draft"):
			drafts++
			http.Error(w, "draft must not be created", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &drafts
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestDataPullMaterialisesLiveBlobs — pull writes the LIVE content of each
// seeded name into BLOBS/<name> (hash-verified), skipping files already
// current.
func TestDataPullMaterialisesLiveBlobs(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "demo", "100", "x", `EMIT .ok = "yes"`)
	// faqs/a.txt drifted to "hola"; faqs/b.txt is current locally.
	hola, hi := []byte("hola"), []byte("hi")
	bDir := filepath.Join(root, "OPS", "demo", "BLOBS", "faqs")
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "b.txt"), hi, 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []client.StackBlob{
		{Name: "faqs/a.txt", SHA256: shaHex(hola), SeededSHA: shaHex([]byte("hello")), Drifted: true},
		{Name: "faqs/b.txt", SHA256: shaHex(hi), SeededSHA: shaHex(hi)},
		{Name: "faqs/new.txt", SHA256: shaHex([]byte("brand new")), SeededSHA: shaHex([]byte("brand new"))},
	}
	srv, _ := fakeBlobAdmin(t, rows, map[string][]byte{
		shaHex(hola): hola, shaHex([]byte("brand new")): []byte("brand new"),
	})
	var out, errb bytes.Buffer
	if code := runDataPull([]string{"--target", srv.URL, "--tenant", "default", root}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	if got, _ := os.ReadFile(filepath.Join(bDir, "a.txt")); string(got) != "hola" {
		t.Fatalf("a.txt = %q, want the live content", got)
	}
	if got, _ := os.ReadFile(filepath.Join(bDir, "new.txt")); string(got) != "brand new" {
		t.Fatalf("new.txt = %q", got)
	}
	if !strings.Contains(out.String(), "3 seeded blobs, 2 updated") {
		t.Fatalf("summary: %s", out.String())
	}
}

// TestDataApplyRefusesDrift — the non-fast-forward rule: a stack whose
// seeded blob drifted is refused before any draft is created.
func TestDataApplyRefusesDrift(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "demo", "100", "x", `EMIT .ok = "yes"`)
	bDir := filepath.Join(root, "OPS", "demo", "BLOBS", "faqs")
	if err := os.MkdirAll(bDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bDir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []client.StackBlob{{Name: "faqs/a.txt", SHA256: shaHex([]byte("hola")), SeededSHA: shaHex([]byte("hello")), Drifted: true}}
	srv, drafts := fakeBlobAdmin(t, rows, nil)
	var out, errb bytes.Buffer
	code := runDataApply([]string{"--target", srv.URL, "--tenant", "default", "--yes", root}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (refused); stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "refused") || !strings.Contains(errb.String(), "faqs/a.txt") ||
		!strings.Contains(errb.String(), "txco data pull") {
		t.Fatalf("refusal message: %s", errb.String())
	}
	if *drafts != 0 {
		t.Fatal("a refused apply created a draft")
	}
}
