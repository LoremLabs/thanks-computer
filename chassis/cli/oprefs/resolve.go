// Package oprefs resolves `op://NAME` symbolic references in txcl rule
// bodies to concrete URLs based on a workspace's operations registry.
//
// Substitution runs at apply time on the CLI side: the chassis only ever
// sees resolved URLs. This keeps the chassis simple and means a `txco
// diff` against an older chassis still works.
package oprefs

import (
	"fmt"
	"regexp"
	"strings"
)

// Operation is the minimum shape resolveOpRefs cares about. The full
// schema (with optional auth fields, etc.) lives in chassis/cli but
// isn't imported here to avoid a cycle.
type Operation struct {
	URL string
}

// opRefRE matches an `op://NAME` reference *inside a double-quoted
// string literal*. The surrounding quotes are part of the match. This
// deliberately excludes:
//
//   - `op://` in `#` line comments  (no leading quote → no match)
//   - bare `op://NAME` outside any string (which would be a txcl parse
//     error anyway)
//   - text matching the inner pattern but not bracketed by quotes
//
// NAME is restricted to a conservative identifier shape (letters,
// digits, underscore, dash). If a real operation name needs richer
// characters, expand the class deliberately rather than by accident.
var opRefRE = regexp.MustCompile(`"op://([A-Za-z0-9_-]+)"`)

// ResolveOpRefs scans txcl for `"op://NAME"` literals and replaces each
// with `"<resolved-url>"` looked up in ops. Returns an error on the first
// unresolved name.
func ResolveOpRefs(txcl string, ops map[string]Operation) (string, error) {
	var unresolved string
	out := opRefRE.ReplaceAllStringFunc(txcl, func(match string) string {
		if unresolved != "" {
			return match // short-circuit; we'll surface the error
		}
		sub := opRefRE.FindStringSubmatch(match)
		name := sub[1]
		op, ok := ops[name]
		if !ok || op.URL == "" {
			unresolved = name
			return match
		}
		return `"` + op.URL + `"`
	})
	if unresolved != "" {
		return "", fmt.Errorf("unresolved op://%s — define it under operations: in txco.yaml (or override under targets.<env>.operations)", unresolved)
	}
	return out, nil
}

// References returns the distinct list of op names referenced inside
// `"op://..."` literals in txcl. Useful for pre-validating a bundle
// before substitution, so a config-shape error surfaces before parse
// validation.
func References(txcl string) []string {
	matches := opRefRE.FindAllStringSubmatch(txcl, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := m[1]
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// HasRefs reports whether txcl contains any `"op://..."` reference.
// Faster than calling References when the caller only needs a yes/no.
func HasRefs(txcl string) bool {
	return strings.Contains(txcl, `"op://`) && opRefRE.MatchString(txcl)
}
