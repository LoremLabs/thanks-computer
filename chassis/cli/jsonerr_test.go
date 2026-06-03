package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestJSONErrWrap(t *testing.T) {
	mk := func(code int, out, errs string) func([]string, io.Writer, io.Writer) int {
		return func(_ []string, o, e io.Writer) int {
			if out != "" {
				_, _ = io.WriteString(o, out)
			}
			if errs != "" {
				_, _ = io.WriteString(e, errs)
			}
			return code
		}
	}

	t.Run("no --json passes through untouched", func(t *testing.T) {
		var o, e bytes.Buffer
		code := jsonErrWrap([]string{"stack"}, &o, &e, mk(1, "", "boom"))
		if code != 1 || o.Len() != 0 || e.String() != "boom" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, o.String(), e.String())
		}
	})

	t.Run("error synthesized as JSON when no result was emitted", func(t *testing.T) {
		var o, e bytes.Buffer
		code := jsonErrWrap([]string{"stack", "--json"}, &o, &e, mk(1, "", "apply: nope"))
		if code != 1 {
			t.Fatalf("code=%d", code)
		}
		if e.Len() != 0 {
			t.Errorf("stderr=%q, want empty (moved into JSON)", e.String())
		}
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(o.Bytes(), &got); err != nil {
			t.Fatalf("stdout not JSON: %v\n%s", err, o.String())
		}
		if got.Error != "apply: nope" {
			t.Errorf("error=%q", got.Error)
		}
	})

	t.Run("existing JSON result (status divergent, exit 1) left intact", func(t *testing.T) {
		var o, e bytes.Buffer
		code := jsonErrWrap([]string{"--json"}, &o, &e, mk(1, "[{\"stack\":\"a\"}]\n", ""))
		if code != 1 {
			t.Fatalf("code=%d", code)
		}
		if strings.Contains(o.String(), `"error"`) {
			t.Errorf("must NOT synthesize an error over an already-emitted result: %q", o.String())
		}
		if !strings.Contains(o.String(), `"stack"`) {
			t.Errorf("original JSON was dropped: %q", o.String())
		}
	})

	t.Run("usage error (exit 2) stays human on stderr", func(t *testing.T) {
		var o, e bytes.Buffer
		code := jsonErrWrap([]string{"--json"}, &o, &e, mk(2, "", "Usage: ..."))
		if code != 2 || o.Len() != 0 || e.String() != "Usage: ..." {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, o.String(), e.String())
		}
	})

	t.Run("success flushes result + stderr progress", func(t *testing.T) {
		var o, e bytes.Buffer
		code := jsonErrWrap([]string{"--json"}, &o, &e, mk(0, "{\"ok\":true}\n", "uploaded x"))
		if code != 0 || !strings.Contains(o.String(), `"ok"`) || e.String() != "uploaded x" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, o.String(), e.String())
		}
	})
}

// TestApplyJSONErrorIsJSON is the end-to-end of the reported bug:
// `txco apply --dry-run --json` with no OPS/ must fail with a JSON error on
// stdout, not human text on stderr.
func TestApplyJSONErrorIsJSON(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	empty := t.TempDir() // no OPS/ here or above

	var o, e bytes.Buffer
	code := jsonErrWrap([]string{empty, "--json", "--dry-run"}, &o, &e, runApply)
	if code != 1 {
		t.Fatalf("code=%d, want 1; stderr=%q", code, e.String())
	}
	if e.Len() != 0 {
		t.Errorf("stderr should be empty in --json mode, got %q", e.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(o.Bytes(), &got); err != nil {
		t.Fatalf("apply --json error was not JSON: %v\n%s", err, o.String())
	}
	if !strings.Contains(got.Error, "no resonators found") {
		t.Errorf("error=%q", got.Error)
	}
}
