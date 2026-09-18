package ops

import (
	"fmt"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/txcguard"
)

// These ops run on the trusted `txco` transport, so the processor merges
// their output WITHOUT the reserved-`_txc` sanitizer an untrusted producer
// gets. Wherever the author picks the path the op writes to, that path is
// therefore the only place to stop `to = "@tenant"` from forging a chassis
// control field — every author-chosen target goes through one of these two.

// authorTarget resolves a WITH param naming where the op writes AUTHOR data
// (txco://copy's `to`). Allowed: the author's own keys and the author-writable
// `_txc` subtrees (`_txc.web.res.*`, …). "" means the param was not given.
func authorTarget(meta []byte, key string) (string, error) {
	raw := gjson.GetBytes(meta, key).String()
	path, ok := txcguard.AuthorTarget(raw)
	if !ok {
		return "", fmt.Errorf("`%s` = %q targets a reserved _txc path", key, raw)
	}
	return path, nil
}

// computedTarget resolves a WITH param naming where an auth helper writes its
// own VERDICT (`output_path`, `configured_path`), falling back to dflt. On top
// of authorTarget's set it allows `_txc.computed.*`, the documented home of
// these results: the value is the op's, not the author's, so choosing the leaf
// forges nothing.
func computedTarget(meta []byte, key, dflt string) (string, error) {
	raw := gjson.GetBytes(meta, key).String()
	path, ok := txcguard.ComputedTarget(raw)
	if !ok {
		return "", fmt.Errorf("`%s` = %q targets a reserved _txc path (use your own key, or one under _txc.computed)", key, raw)
	}
	if path == "" {
		path = dflt
	}
	return path, nil
}
