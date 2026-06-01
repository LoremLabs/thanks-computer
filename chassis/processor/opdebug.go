package processor

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// opDebugField is the well-known top-level key any EXEC handler can
// stamp on its response payload to surface diagnostic data. Underscore-
// separated (not under `_txc.`) is deliberate: `_txc.*` is the
// propagating envelope namespace; `_txc_op_debug` is explicitly NOT
// propagating — the chassis strips it before merge.
//
// **Where debug content lives:** the per-step `out.json` trace file.
// Step output is captured inside `pu.Exec` at processor.go:2417-2425
// (with EMIT overlay applied to mirror the op's actual contribution).
// That capture happens BEFORE the post-Exec strip in processor.go:701
// runs, so the step's `out.json` already contains `_txc_op_debug` as
// the handler stamped it. The chassis then strips the field before the
// envelope merge so rules cannot read it. No separate timeline event is
// needed — the data is already in step.out, and `_txc_op_debug` is
// grep-friendly across step files.
//
// To find debug content for a request:
//
//	grep -l '_txc_op_debug' .txco/dev/trace/requests/<rid>/steps/*/out.json
//
// or via the admin trace UI per-step expansion.
const opDebugField = "_txc_op_debug"

// extractOpDebug pulls `_txc_op_debug` off raw (if present), returning
// the stripped raw + a presence flag. Same pattern as the existing
// `_txc.goto` / `_txc.halt` extraction in advanceAfterScope —
// handler-stamped control fields that the chassis owns and consumes
// BEFORE the envelope merge.
//
// The stripped content is discarded HERE (it's already captured in the
// step's `out.json` by the pre-strip trace capture). Returning the
// presence flag lets the caller no-op cleanly when nothing was stamped.
func extractOpDebug(raw string) (stripped string, present bool) {
	if !strings.Contains(raw, opDebugField) {
		return raw, false
	}
	r := gjson.Get(raw, opDebugField)
	if !r.Exists() {
		return raw, false
	}
	stripped, err := sjson.Delete(raw, opDebugField)
	if err != nil {
		// Defensive: if the delete fails (shouldn't, on valid JSON),
		// return raw unchanged. Better to leak the field into the
		// envelope than swallow it silently — operators will spot it.
		return raw, true
	}
	return stripped, true
}
