package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestDiffHonorsFlagsAfterPositional guards the flag-order fix that motivated
// the pflag switch: `txco diff test-01 --json` must honor --json even though it
// trails the <dir> positional. We confirm a trailing --addr is parsed (the
// unreachable-chassis error echoes it) — same parse path as --json.
func TestDiffHonorsFlagsAfterPositional(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runDiff([]string{dir, "--addr", "http://order-check.invalid:9"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 (unreachable), got %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "order-check.invalid") {
		t.Fatalf("--addr after the <dir> positional was ignored: %q", stderr.String())
	}
}

func TestEmitDiffJSON(t *testing.T) {
	drifts := []stackDrift{
		{Stack: "hello", Remote: "v3", Local: "v3 (clean)", Note: "in sync", URL: "https://hello.acme.com"},
		{Stack: "test-01", Remote: "v5", Local: "untracked", Note: "no local state", Divergent: true},
	}
	files := []diffFileChange{
		{Stack: "hello", Scope: 100, Name: "greet", Kind: "change"},
		{Stack: "test-01", Scope: 0, Name: "root", Kind: "add"},
	}
	var out, errBuf bytes.Buffer
	if code := emitDiffJSON(&out, &errBuf, drifts, files); code != 0 {
		t.Fatalf("exit=%d, want 0 (diff is a preview, not a probe)", code)
	}

	var got struct {
		Stacks []struct {
			Stack     string `json:"stack"`
			URL       string `json:"url"`
			Divergent bool   `json:"divergent"`
		} `json:"stacks"`
		Files []struct {
			Stack string `json:"stack"`
			Scope int    `json:"scope"`
			Name  string `json:"name"`
			Kind  string `json:"kind"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(got.Stacks) != 2 || got.Stacks[0].URL != "https://hello.acme.com" {
		t.Errorf("stacks = %+v", got.Stacks)
	}
	if len(got.Files) != 2 || got.Files[1].Kind != "add" || got.Files[0].Scope != 100 {
		t.Errorf("files = %+v", got.Files)
	}
}

func TestEmitDiffJSONEmpty(t *testing.T) {
	// No drifts, no changes → stable shape with empty arrays (not null).
	var out, errBuf bytes.Buffer
	if code := emitDiffJSON(&out, &errBuf, nil, nil); code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
	s := out.String()
	if !strings.Contains(s, `"stacks": []`) || !strings.Contains(s, `"files": []`) {
		t.Errorf("empty diff should emit empty arrays, not null:\n%s", s)
	}
}
