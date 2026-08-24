package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
)

func TestRemoteMoved(t *testing.T) {
	i := func(n int64) *int64 { return &n }
	if remoteMoved(nil, &client.StackRecord{ActiveVersion: i(9)}) != nil {
		t.Fatal("no baseline must not refuse")
	}
	if remoteMoved(&state.State{VersionNumber: 5}, nil) != nil {
		t.Fatal("no record must not refuse")
	}
	if remoteMoved(&state.State{VersionNumber: 5}, &client.StackRecord{ActiveVersion: i(5)}) != nil {
		t.Fatal("in sync must not refuse")
	}
	m := remoteMoved(&state.State{VersionNumber: 5}, &client.StackRecord{ActiveVersion: i(7)})
	if m == nil || m.Active != 7 || m.Synced != 5 || m.Rollback {
		t.Fatalf("ahead: %+v", m)
	}
	m = remoteMoved(&state.State{VersionNumber: 7}, &client.StackRecord{ActiveVersion: i(3)})
	if m == nil || !m.Rollback {
		t.Fatalf("rollback: %+v", m)
	}
	if m := remoteMoved(&state.State{VersionNumber: 2}, &client.StackRecord{}); m == nil || m.Active != 0 {
		t.Fatalf("deactivated stack: %+v", m)
	}
	msg := refusedMovedMessage("apply", "demo", &remoteMove{Synced: 5, Active: 7})
	for _, want := range []string{"refused", "v7", "v5", "txco diff demo", "txco pull demo --force", "apply --force"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message lacks %q: %s", want, msg)
		}
	}
	if n := expectedActiveFor(&client.StackRecord{ActiveVersion: i(4)}); n == nil || *n != 4 {
		t.Fatal("expectedActiveFor")
	}
	if n := expectedActiveFor(&client.StackRecord{}); n == nil || *n != 0 {
		t.Fatal("expectedActiveFor none")
	}
}

func TestRecordSyncedVersionKeepsManifest(t *testing.T) {
	root := t.TempDir()
	if err := state.Save(root, "demo", state.State{VersionNumber: 3, ParentVersionNumber: 3, ManifestHash: "m"}); err != nil {
		t.Fatal(err)
	}
	recordSyncedVersion(root, "demo", 9)
	s, _ := state.Load(root, "demo")
	if s == nil || s.VersionNumber != 9 || s.ParentVersionNumber != 9 || s.ManifestHash != "m" {
		t.Fatalf("state = %+v", s)
	}
	recordSyncedVersion(root, "fresh", 2)
	if s, _ := state.Load(root, "fresh"); s == nil || s.VersionNumber != 2 || s.ManifestHash != "" {
		t.Fatalf("fresh state = %+v", s)
	}
}

// fakeMovedAdmin answers the stack lookup with active v7 and counts draft
// creations; the bulk list fails so apply falls back to the per-stack probe.
func fakeMovedAdmin(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	drafts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks":
			http.Error(w, "no bulk list here", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks/demo":
			v := int64(7)
			_ = json.NewEncoder(w).Encode(client.StackRecord{Name: "demo", ActiveVersion: &v, ManifestHash: "zzz", CodeManifestHash: "zzz"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks/demo/blobs":
			_ = json.NewEncoder(w).Encode(map[string]any{"stack": "demo", "names": []any{}, "count": 0})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/draft"):
			drafts++
			http.Error(w, `{"error":"stop_here"}`, http.StatusBadRequest) // non-retryable: the test only needs to see the pipeline reach the draft
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &drafts
}

// TestApplyRefusesWhenChassisMoved — the code path's non-fast-forward
// guard: the workspace last synced v5, the chassis is at v7 → refused
// before any draft is created; --force proceeds (and reaches the draft).
func TestApplyRefusesWhenChassisMoved(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "demo", "100", "x", `EMIT .ok = "yes"`)
	if err := state.Save(root, "demo", state.State{VersionNumber: 5, ParentVersionNumber: 5, ManifestHash: "old"}); err != nil {
		t.Fatal(err)
	}
	srv, drafts := fakeMovedAdmin(t)
	var out, errb bytes.Buffer
	code := runApply([]string{"--target", srv.URL, "--tenant", "default", "--yes", root}, &out, &errb)
	if code == 0 {
		t.Fatalf("exit=%d, want non-zero (refused); stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "refused") || !strings.Contains(errb.String(), "v7") || !strings.Contains(errb.String(), "v5") {
		t.Fatalf("refusal message: %s", errb.String())
	}
	if *drafts != 0 {
		t.Fatal("refused apply created a draft")
	}
	// --force: the guard is bypassed and the pipeline proceeds to the draft.
	out.Reset()
	errb.Reset()
	_ = runApply([]string{"--target", srv.URL, "--tenant", "default", "--yes", "--force", root}, &out, &errb)
	if *drafts != 1 {
		t.Fatalf("--force must proceed to draft creation (drafts=%d; stderr=%s)", *drafts, errb.String())
	}
}

func TestDataApplyRefusesWhenChassisMoved(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	writeStackFixture(t, root, "demo", "100", "x", `EMIT .ok = "yes"`)
	kvDir := filepath.Join(root, "OPS", "demo", "KV")
	if err := os.MkdirAll(kvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kvDir, "cfg.jsonl"), []byte(`{"key":"k","value":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(root, "demo", state.State{VersionNumber: 5, ParentVersionNumber: 5}); err != nil {
		t.Fatal(err)
	}
	srv, drafts := fakeMovedAdmin(t)
	var out, errb bytes.Buffer
	if code := runDataApply([]string{"--target", srv.URL, "--tenant", "default", "--yes", root}, &out, &errb); code != 1 {
		t.Fatalf("exit=%d, want 1; stderr=%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "refused") || *drafts != 0 {
		t.Fatalf("refusal: drafts=%d stderr=%s", *drafts, errb.String())
	}
}
