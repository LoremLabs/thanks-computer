package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// writeJSON encodes v as indented JSON (trailing newline) to w. Shared by the
// `--json` paths of the stack/deploy commands (status, diff, versions, pull,
// apply, push, draft, activate) so they emit a consistent machine-readable
// shape on stdout.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// wantsJSON reports whether args request --json output (in any position, in
// either single- or double-dash form). Used to decide whether failures should
// be rendered as JSON rather than human text.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			break // end of flags
		}
		if a == "--json" || a == "-json" ||
			strings.HasPrefix(a, "--json=") || strings.HasPrefix(a, "-json=") {
			return true
		}
	}
	return false
}

// jsonErrWrap runs a subcommand and, when --json was requested, guarantees the
// output stays machine-readable on failure: an operational error (exit 1) that
// the command reported on stderr WITHOUT having already emitted a JSON result
// is re-rendered as a `{"error": …}` object on stdout — so `txco <cmd> --json |
// jq` always receives valid JSON. The "without a JSON result" guard
// (outBuf.Len()==0) deliberately leaves commands that already printed their
// JSON alone, including `status`'s exit-1 "divergent" signal. Usage/flag errors
// (exit 2) and the human path are passed through untouched.
func jsonErrWrap(args []string, stdout, stderr io.Writer, run func([]string, io.Writer, io.Writer) int) int {
	if !wantsJSON(args) {
		return run(args, stdout, stderr)
	}
	var outBuf, errBuf bytes.Buffer
	code := run(args, &outBuf, &errBuf)
	_, _ = stdout.Write(outBuf.Bytes())
	if code == 1 && outBuf.Len() == 0 {
		_ = writeJSON(stdout, map[string]string{"error": strings.TrimSpace(errBuf.String())})
	} else {
		_, _ = stderr.Write(errBuf.Bytes())
	}
	return code
}
