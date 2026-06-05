package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, map[string]int{"v": 3}); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "{\n  \"v\": 3\n}" {
		t.Errorf("unexpected encoding: %q", got)
	}
}

// deployChassis is a minimal httptest stand-in that answers the four admin
// calls a deploy makes (draft → files → validate → activate), routing by URL
// suffix. version 3 supersedes prior active 2.
func deployChassis(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = w.Write([]byte(`{"version_number":3}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write([]byte(`{"manifest_hash":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_, _ = w.Write([]byte(`{"ok":true,"checked":1}`))
		case strings.HasSuffix(r.URL.Path, "/activate"):
			_, _ = w.Write([]byte(`{"version_number":3,"prior_version_number":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

type deployJSON struct {
	Stack        string `json:"stack"`
	Version      int64  `json:"version"`
	PriorVersion *int64 `json:"prior_version"`
	Files        int    `json:"files"`
	Activated    bool   `json:"activated"`
}

func TestRunPushJSON(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)
	srv := deployChassis(t)
	defer srv.Close()

	var out, errb bytes.Buffer
	code := runPush([]string{"api", root, "--addr", srv.URL, "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	// push deploys one stack → a single JSON object (not an array).
	var got deployJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, out.String())
	}
	if got.Stack != "api" || got.Version != 3 || !got.Activated || got.Files != 1 {
		t.Errorf("got %+v", got)
	}
	if got.PriorVersion == nil || *got.PriorVersion != 2 {
		t.Errorf("prior_version = %v, want 2", got.PriorVersion)
	}
}

// TestRunPushRecordsLocalState proves a successful push now writes the local
// state file so `txco status` reports the stack in sync (the prior behavior:
// "no local state recorded — run `txco pull`"). Crucially the recorded
// ManifestHash must equal what drift/status recomputes from the on-disk OPS
// tree, so the stack reads "(clean)" immediately after push.
func TestRunPushRecordsLocalState(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)
	srv := deployChassis(t)
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := runPush([]string{"api", root, "--addr", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d; stderr=%q", code, errb.String())
	}

	st, err := state.Load(root, "api")
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if st == nil {
		t.Fatal("push did not record local state (.txco/api.state.json missing)")
	}
	if st.VersionNumber != 3 || st.ParentVersionNumber != 3 {
		t.Errorf("state versions = %d/%d, want 3/3 (the activated version)", st.VersionNumber, st.ParentVersionNumber)
	}
	// Status (drift) recomputes localManifestHash(loadLocalStackFiles(...)); the
	// recorded hash must match it so the stack reads "(clean)" right after push.
	localFiles, ferr := loadLocalStackFiles(filepath.Join(root, "OPS", "api"))
	if ferr != nil {
		t.Fatalf("loadLocalStackFiles: %v", ferr)
	}
	if want := localManifestHash(localFiles); st.ManifestHash != want {
		t.Errorf("recorded ManifestHash %q != status's recompute %q — status would show 'edited' right after push", st.ManifestHash, want)
	}
}

func TestRunApplyJSON(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)
	srv := deployChassis(t)
	defer srv.Close()

	var out, errb bytes.Buffer
	code := runApply([]string{root, "--addr", srv.URL, "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d, want 0; stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	// apply deploys the whole workspace → a JSON array (one entry per stack).
	var got []deployJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out.String())
	}
	if len(got) != 1 || got[0].Stack != "api" || got[0].Version != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestRunVersionsJSON(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/versions") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"versions":[` +
			`{"version_number":2,"status":"active","is_active":true,"created_by":"me","created_at":"t1"},` +
			`{"version_number":1,"status":"superseded","created_by":"me","created_at":"t0"}]}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := runVersions([]string{"api", "--addr", srv.URL, "--json"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit=%d; stderr=%q", code, errb.String())
	}
	var got []struct {
		VersionNumber int64 `json:"version_number"`
		IsActive      bool  `json:"is_active"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out.String())
	}
	if len(got) != 2 || got[0].VersionNumber != 2 || !got[0].IsActive {
		t.Errorf("got %+v", got)
	}
}
