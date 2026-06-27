package auth

import "strings"

// looksLikeURL reports whether a --target value is a raw admin endpoint (a
// scheme like "http://…" or a host:port — both contain ":") rather than a bare
// profile name ("dev", "cloud"). Mirrors the cli package's helper; duplicated
// here to avoid an import cycle (cli imports auth, not the reverse).
func looksLikeURL(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && strings.Contains(v, ":")
}

// applyTargetSelector folds the unified --target flag into a command's existing
// --url / --profile (or --name) pointers, so the established resolution path
// (resolveProfileForTenant + buildSignedTarget) keeps working unchanged.
//
// --target is the canonical chassis selector across the whole CLI:
//   - a raw admin URL  → behaves like --url
//   - a profile name   → behaves like --profile (a "named chassis")
//
// An explicitly-passed --url / --profile is left untouched (so a power user can
// still mix, e.g. --target <url> --profile <key>). Call once, after fs.Parse.
func applyTargetSelector(targetSel string, url, profile *string) {
	t := strings.TrimSpace(targetSel)
	if t == "" {
		return
	}
	if looksLikeURL(t) {
		if strings.TrimSpace(*url) == "" {
			*url = t
		}
		return
	}
	if strings.TrimSpace(*profile) == "" {
		*profile = t
	}
}

// applyTargetSelectorName is applyTargetSelector for the identity commands whose
// signing-key flag (--name) carries a non-empty DEFAULT (defaultKeyName). A
// --target naming a profile overrides that default outright (you asked for a
// specific chassis), where applyTargetSelector would have left the default in
// place.
func applyTargetSelectorName(targetSel string, url, name *string) {
	t := strings.TrimSpace(targetSel)
	if t == "" {
		return
	}
	if looksLikeURL(t) {
		if strings.TrimSpace(*url) == "" {
			*url = t
		}
		return
	}
	*name = t
}
