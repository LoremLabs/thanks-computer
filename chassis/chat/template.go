package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// Render substitutes `{{@field.path}}` markers in template with values
// from the envelope. The `@`-prefix follows the **chassis-wide txcl
// convention**: it addresses paths under the envelope's `_txc.*`
// namespace, matching what `WHEN @path`, `SET @path = ...`, and
// `EMIT @path = ...` already do (the txcl parser rewrites `@x.y` to
// `_txc.x.y` at parse time — see chassis/txcl/parser/parser.go:216,234).
// So `{{@web.req.body}}` reads `_txc.web.req.body`. For a scratch value
// the author supplies (e.g. `{{@body_text}}` -> `_txc.body_text`), set it
// with `SET PRE @body_text = …`: SET PRE decorates only this op's input,
// so the template sees it but it never propagates to the next scope.
// Note `_txc.*` is the chassis control namespace — reserved fields
// (tenant, computed.*, budget, …) are NOT author-writable on the
// propagating envelope (see chassis/processor authorMayWriteTxc); the
// op-local SET PRE scratch above is exempt because it doesn't merge.
//
// The substitution is JSON-escaped by default: string values are
// JSON-string-escaped with the outer quotes stripped, so they splice
// safely into the surrounding prompt context (no terminator breaks;
// no injection of quotes or backslashes that would mangle the prompt).
// Non-string values (objects, numbers, arrays, booleans, nulls) are
// marshaled to compact JSON.
//
// Unknown paths render as the empty string and emit one debug log per
// occurrence (prompts tolerate missing data gracefully — erroring would
// turn every type-ahead-incomplete envelope into a request failure).
// Logger may be nil.
//
// The verbatim opt-out form `{{!@path}}` is **intentionally rejected**
// in v1: any template containing it returns InvalidWithError. Trusted-
// fields-only insertion is the kind of escalation that wants its own
// review, and silently accepting it without escaping would be the wrong
// failure mode when the feature lands. Failing loud here means authors
// using the syntax today get a clear error, not the wrong escaping.
//
// Prompt-injection caveat: the chassis cannot tell trusted from hostile
// input. The escaping makes hostile JSON inside a string value safe
// against trivial prompt-context breaks; it does NOT defend against an
// adversary who tells the model to ignore prior instructions. Fence-
// and-instruct is the recommended pattern; v2 may add a sanitize knob.
func Render(template string, envelope []byte, logger *zap.Logger) (string, error) {
	if !strings.Contains(template, "{{") {
		return template, nil
	}

	var out strings.Builder
	out.Grow(len(template))

	i := 0
	for i < len(template) {
		// Find next {{.
		start := strings.Index(template[i:], "{{")
		if start < 0 {
			out.WriteString(template[i:])
			break
		}
		start += i

		// Copy literal prefix.
		out.WriteString(template[i:start])

		// Find matching }}. We don't support nested {{; the scanner is
		// greedy on the first }}.
		closeRel := strings.Index(template[start+2:], "}}")
		if closeRel < 0 {
			// No closer; emit the unconsumed remainder verbatim. Authors
			// see exactly what they wrote, including the unclosed `{{`.
			out.WriteString(template[start:])
			break
		}
		end := start + 2 + closeRel

		body := strings.TrimSpace(template[start+2 : end])

		switch {
		case strings.HasPrefix(body, "!@"):
			// Reject verbatim form. Authors might be reaching for it
			// thinking they want raw JSON; either way, the v1 boundary
			// is escaped-only and silently honoring it would be wrong.
			return "", &InvalidWithError{
				Reason: "verbatim template form {{!@...}} is not supported in v1",
				Detail: map[string]string{
					"marker":   template[start : end+2],
					"feature":  "raw_template_insertion",
					"deferred": "v1.1",
				},
			}
		case strings.HasPrefix(body, "@"):
			path := strings.TrimSpace(body[1:])
			if path == "" {
				return "", &InvalidWithError{
					Reason: "empty path in template marker",
					Detail: map[string]string{"marker": template[start : end+2]},
				}
			}
			rendered, ok := lookup(envelope, path)
			if !ok && logger != nil {
				logger.Debug("chat template: missing envelope path",
					zap.String("path", path))
			}
			out.WriteString(rendered)
		default:
			// {{...}} without leading @ — not a recognized marker. v1
			// rejects so authors don't ship a typo silently. (Future
			// `{{#if ...}}` style helpers can add their own prefixes.)
			return "", &InvalidWithError{
				Reason: "unrecognized template marker (expected {{@path}})",
				Detail: map[string]string{"marker": template[start : end+2]},
			}
		}

		i = end + 2
	}

	return out.String(), nil
}

// lookup resolves a path against envelope and returns the escaped
// substitution string + whether the path existed. Matches the txcl
// `@path → _txc.path` rewrite (parser.go:216,234) so authors get the
// same mental model in templates as in WHEN/SET/EMIT.
func lookup(envelope []byte, path string) (string, bool) {
	r := gjson.GetBytes(envelope, "_txc."+path)
	if !r.Exists() {
		return "", false
	}
	if r.Type == gjson.String {
		// JSON-encode the string then strip the outer quotes. This gives
		// us the escape behavior (\n, \t, \", \\) without re-quoting the
		// substitution at the splice site.
		b, err := json.Marshal(r.String())
		if err != nil {
			return "", true
		}
		s := string(b)
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1], true
		}
		return s, true
	}
	// Non-string: emit the raw JSON form. gjson's Raw is the original
	// bytes from envelope, which is already compact JSON.
	if r.Raw != "" {
		return r.Raw, true
	}
	// Fallback: stringify and re-marshal (rare; covers fabricated values
	// without a Raw backing).
	return fmt.Sprintf("%v", r.Value()), true
}
