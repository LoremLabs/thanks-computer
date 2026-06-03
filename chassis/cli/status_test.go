package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitStatusJSON(t *testing.T) {
	drifts := []stackDrift{
		{Stack: "hello", Remote: "v3", Local: "v3 (clean)", Note: "in sync", URL: "https://hello.acme.com"},
		{Stack: "test-01", Remote: "v5", Local: "untracked",
			Note: "no local state recorded — run `txco pull test-01`", Divergent: true},
	}
	var out, errBuf bytes.Buffer
	code := emitStatusJSON(&out, &errBuf, drifts)

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (a stack is divergent)", code)
	}
	var got []struct {
		Stack     string `json:"stack"`
		Remote    string `json:"remote"`
		Local     string `json:"local"`
		URL       string `json:"url"`
		Note      string `json:"note"`
		Divergent bool   `json:"divergent"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Stack != "hello" || got[0].URL != "https://hello.acme.com" || got[0].Divergent {
		t.Errorf("row0 = %+v", got[0])
	}
	if got[1].Stack != "test-01" || !got[1].Divergent {
		t.Errorf("row1 = %+v", got[1])
	}
	// The URL-less divergent row should omit the url key (omitempty).
	if strings.Contains(strings.Split(out.String(), "test-01")[1], `"url"`) {
		t.Errorf("expected url omitted for the URL-less stack:\n%s", out.String())
	}
}

// TestStatusHonorsFlagsAfterPositional guards the flag-order fix. With Go's
// stdlib flag package, a flag after the optional <dir> positional (e.g.
// `txco status somedir --json`) was silently dropped — it stops parsing at the
// first non-flag arg. pflag parses flags in any position; we prove it by
// confirming an --addr that TRAILS the positional actually targets that
// address (the unreachable-chassis error echoes it) rather than localhost.
func TestStatusHonorsFlagsAfterPositional(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runStatus([]string{dir, "--addr", "http://order-check.invalid:9"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 (unreachable), got %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "order-check.invalid") {
		t.Fatalf("--addr after the <dir> positional was ignored: %q", stderr.String())
	}
}

func TestEmitStatusJSONEmptyInSync(t *testing.T) {
	var out, errBuf bytes.Buffer
	// No stacks → empty array, exit 0.
	if code := emitStatusJSON(&out, &errBuf, nil); code != 0 {
		t.Errorf("empty: exit=%d, want 0", code)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("empty workspace should emit [], got %q", out.String())
	}

	// All in-sync → exit 0.
	out.Reset()
	code := emitStatusJSON(&out, &errBuf, []stackDrift{{Stack: "a", Remote: "v1", Local: "v1 (clean)", Note: "in sync"}})
	if code != 0 {
		t.Errorf("in-sync: exit=%d, want 0", code)
	}
}
