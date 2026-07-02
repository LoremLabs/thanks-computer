package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
		case strings.HasSuffix(r.URL.Path, "/stacks"):
			// Bulk list (default apply's fast-skip probe). Empty → no stack matches,
			// so each falls through to the per-stack GetStack no-op / normal push.
			_, _ = w.Write([]byte(`{"stacks":[]}`))
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
	localFiles, ferr := loadLocalStackFiles(root, "api")
	if ferr != nil {
		t.Fatalf("loadLocalStackFiles: %v", ferr)
	}
	if want := localManifestHash(localFiles); st.ManifestHash != want {
		t.Errorf("recorded ManifestHash %q != status's recompute %q — status would show 'edited' right after push", st.ManifestHash, want)
	}
}

// TestRunApplyChanged proves `apply --changed` skips a stack whose local code
// matches the digest recorded by the previous apply — with NO server round-trip
// (not even the GetStack probe: the counting server 404s GetStack, so the default
// path would create a draft) — and re-applies once the code actually changes.
func TestRunApplyChanged(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)

	var drafts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/draft"):
			atomic.AddInt32(&drafts, 1)
			_, _ = w.Write([]byte(`{"version_number":3}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write([]byte(`{"manifest_hash":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_, _ = w.Write([]byte(`{"ok":true,"checked":1}`))
		case strings.HasSuffix(r.URL.Path, "/activate"):
			_, _ = w.Write([]byte(`{"version_number":3,"prior_version_number":2}`))
		case strings.HasSuffix(r.URL.Path, "/stacks"):
			_, _ = w.Write([]byte(`{"stacks":[]}`)) // bulk list empty → per-stack path
		default:
			http.NotFound(w, r) // GetStack 404s → default path falls through to a real push
		}
	}))
	defer srv.Close()

	// First apply (default mode) records .txco/api.state.json (its ManifestHash).
	var out, errb bytes.Buffer
	if code := runApply([]string{root, "--addr", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("first apply exit=%d; stderr=%q", code, errb.String())
	}
	if got := atomic.LoadInt32(&drafts); got != 1 {
		t.Fatalf("first apply created %d drafts, want 1", got)
	}

	// Second apply --changed: code unchanged → skipped, no server draft.
	out.Reset()
	errb.Reset()
	if code := runApply([]string{root, "--addr", srv.URL, "--changed"}, &out, &errb); code != 0 {
		t.Fatalf("--changed apply exit=%d; stderr=%q", code, errb.String())
	}
	if got := atomic.LoadInt32(&drafts); got != 1 {
		t.Errorf("--changed re-drafted an unchanged stack: drafts=%d, want 1", got)
	}

	// Edit the stack → --changed must re-apply it.
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "no"`)
	out.Reset()
	errb.Reset()
	if code := runApply([]string{root, "--addr", srv.URL, "--changed"}, &out, &errb); code != 0 {
		t.Fatalf("--changed after edit exit=%d; stderr=%q", code, errb.String())
	}
	if got := atomic.LoadInt32(&drafts); got != 2 {
		t.Errorf("--changed skipped a modified stack: drafts=%d, want 2", got)
	}
}

// TestRunApplyDefaultBulkSkip proves the DEFAULT apply skips a stack whose
// server manifest (from the single bulk GET /stacks) already matches the local
// code — with NO per-stack GetStack and NO draft. Server-authoritative: the skip
// keys off the server's live hash, not a local record.
func TestRunApplyDefaultBulkSkip(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)

	// The hash the server must report to look "unchanged" == what apply computes.
	files, err := loadLocalStackFiles(root, "api")
	if err != nil {
		t.Fatalf("loadLocalStackFiles: %v", err)
	}
	serverHash := localManifestHash(files)

	var drafts, getStacks int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/draft"):
			atomic.AddInt32(&drafts, 1)
			_, _ = w.Write([]byte(`{"version_number":3}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write([]byte(`{"manifest_hash":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_, _ = w.Write([]byte(`{"ok":true,"checked":1}`))
		case strings.HasSuffix(r.URL.Path, "/activate"):
			_, _ = w.Write([]byte(`{"version_number":3,"prior_version_number":2}`))
		case strings.HasSuffix(r.URL.Path, "/stacks"):
			_, _ = w.Write([]byte(`{"stacks":[{"name":"api","active_version":2,"manifest_hash":"` + serverHash + `"}]}`))
		case strings.Contains(r.URL.Path, "/stacks/"):
			atomic.AddInt32(&getStacks, 1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := runApply([]string{root, "--addr", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("apply exit=%d; stderr=%q", code, errb.String())
	}
	if got := atomic.LoadInt32(&drafts); got != 0 {
		t.Errorf("default apply drafted an unchanged stack: drafts=%d, want 0", got)
	}
	if got := atomic.LoadInt32(&getStacks); got != 0 {
		t.Errorf("default apply made a per-stack GetStack despite a bulk-list match: %d, want 0", got)
	}
}

// TestRunApplyForce proves --force re-versions a stack the default would skip:
// the bulk list reports a matching hash, yet --force drafts+activates anyway.
func TestRunApplyForce(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "api", "0", "root", `EMIT .ok = "yes"`)

	files, err := loadLocalStackFiles(root, "api")
	if err != nil {
		t.Fatalf("loadLocalStackFiles: %v", err)
	}
	serverHash := localManifestHash(files) // would trigger a default skip

	var drafts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/draft"):
			atomic.AddInt32(&drafts, 1)
			_, _ = w.Write([]byte(`{"version_number":3}`))
		case strings.HasSuffix(r.URL.Path, "/files"):
			_, _ = w.Write([]byte(`{"manifest_hash":"abc"}`))
		case strings.HasSuffix(r.URL.Path, "/validate"):
			_, _ = w.Write([]byte(`{"ok":true,"checked":1}`))
		case strings.HasSuffix(r.URL.Path, "/activate"):
			_, _ = w.Write([]byte(`{"version_number":3,"prior_version_number":2}`))
		case strings.HasSuffix(r.URL.Path, "/stacks"):
			_, _ = w.Write([]byte(`{"stacks":[{"name":"api","active_version":2,"manifest_hash":"` + serverHash + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := runApply([]string{root, "--addr", srv.URL, "--force"}, &out, &errb); code != 0 {
		t.Fatalf("--force apply exit=%d; stderr=%q", code, errb.String())
	}
	if got := atomic.LoadInt32(&drafts); got != 1 {
		t.Errorf("--force skipped an unchanged stack: drafts=%d, want 1", got)
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
