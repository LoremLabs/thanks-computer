package ops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// Copy is the handler for `txco://copy`. It reads a value from one
// envelope path and writes it to another, with optional encoding.
//
// This is the chassis-side answer to "txcl SET RHS is literal-only"
// (the constraint that forces `txco://route` to exist in Go). When a
// rule needs to move an envelope field — e.g. "the response body for
// this web request lives at .text, please put it at
// _txc.web.res.body" — Copy is the primitive.
//
// WITH parameters (op.Meta):
//
//	from    = ".text"                 (required: source path on input envelope)
//	to      = "_txc.web.res.body"     (required: destination path on response)
//	encode  = "base64"                (optional: "base64" | "" — default "")
//	default = "fallback value"        (optional: literal substituted when
//	                                  the source path is empty/missing)
//
// Path syntax follows gjson on read (a leading "." is optional and
// stripped) and sjson on write. When the source path is absent or
// resolves to an empty value AND `default` is set, the literal
// `default` value is used as the source instead — letting one rule
// express "use query param if present, else fall back to this."
// Without `default`, an empty source produces an empty destination
// (no failure). A missing `from` or `to` parameter at the WITH
// level IS an authoring error and fails loud.
func Copy(ctx context.Context, _ string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))

	fromRaw := gjson.GetBytes(meta, "from").String()
	toRaw := gjson.GetBytes(meta, "to").String()
	encode := gjson.GetBytes(meta, "encode").String()

	if fromRaw == "" {
		return errPayload("copy: missing `from` in WITH"), errors.New("copy: missing `from`")
	}
	if toRaw == "" {
		return errPayload("copy: missing `to` in WITH"), errors.New("copy: missing `to`")
	}

	from := normalizePath(fromRaw)
	to := normalizePath(toRaw)

	// Read the source. `.String()` on a missing path returns "" —
	// legitimate "copy whatever is there" semantic. For non-string
	// types we fall back to `.Raw` so callers can copy arrays or
	// objects too.
	src := gjson.GetBytes(in, from)
	var value string
	srcIsStructured := false
	switch src.Type {
	case gjson.String:
		value = src.String()
	default:
		// numbers, booleans, arrays, objects, null — preserve shape.
		value = src.Raw
		if src.Raw != "" {
			srcIsStructured = true
		}
	}

	// `default` fallback: if the source resolved to an empty
	// string/missing AND a default was supplied, substitute it. The
	// default is a string literal — for structured defaults the
	// caller is better off with a SET-pre rule.
	if value == "" {
		if def := gjson.GetBytes(meta, "default"); def.Exists() {
			value = def.String()
			srcIsStructured = false // default is always a string literal
		}
	}

	switch encode {
	case "":
		// raw passthrough
	case "base64":
		value = base64.StdEncoding.EncodeToString([]byte(value))
	default:
		return errPayload(fmt.Sprintf("copy: unsupported encode %q (base64|empty)", encode)),
			fmt.Errorf("copy: unsupported encode %q", encode)
	}

	resp := `{}`
	// When encoding is empty AND the source was a structured type
	// (object/array/number/bool/null), SetRaw preserves the
	// structure. Otherwise (string, encoded, or default-substituted)
	// we set as a string.
	if encode == "" && srcIsStructured {
		resp, _ = sjson.SetRaw(resp, to, value)
	} else {
		resp, _ = sjson.Set(resp, to, value)
	}

	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// normalizePath converts a txcl-style envelope path into the
// dotted form gjson/sjson expect. Two shorthands are recognized:
//
//   - leading `@`  → `_txc.` (the same sugar txcl's WHEN parser
//     applies, so a rule author can write `@web.req.url...` in
//     WITH params and have it resolve to `_txc.web.req...`)
//   - leading `.`  → stripped (gjson/sjson paths don't take a
//     leading dot)
//
// Internal paths (no prefix) pass through unchanged.
func normalizePath(p string) string {
	if strings.HasPrefix(p, "@") {
		return "_txc." + strings.TrimPrefix(p, "@")
	}
	return strings.TrimPrefix(p, ".")
}
