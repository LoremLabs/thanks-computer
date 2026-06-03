package update

import (
	"context"
	"strings"

	"golang.org/x/mod/semver"
)

// Result is the outcome of an update check.
type Result struct {
	Current    string // running version, e.g. "0.2.3"
	Latest     string // latest release version, no leading v, e.g. "0.2.6"
	HTMLURL    string // release page URL
	Available  bool   // Latest is strictly newer than Current
	Comparable bool   // Current parsed as valid semver (false ⇒ dev/unset)
}

// Check fetches the latest release and compares it against the current
// version. userAgent should carry the CLI version (e.g. "txco-cli/0.2.3").
func Check(ctx context.Context, current, userAgent string) (Result, error) {
	rel, err := LatestRelease(ctx, userAgent)
	if err != nil {
		return Result{}, err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	res := Result{
		Current: current,
		Latest:  latest,
		HTMLURL: rel.HTMLURL,
	}
	cv, lv := ensureV(current), ensureV(latest)
	if semver.IsValid(cv) && semver.IsValid(lv) {
		res.Comparable = true
		res.Available = semver.Compare(lv, cv) > 0
	}
	return res, nil
}

// ensureV normalizes a version string to the leading-v form golang.org/x/
// mod/semver expects ("0.2.3" → "v0.2.3"); empty stays empty (invalid).
func ensureV(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}
