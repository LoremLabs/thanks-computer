package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Policy is the client-version policy a server advertises in its /healthz
// JSON. Empty fields mean "unset" (the server has no opinion).
type Policy struct {
	Latest           string `json:"latest"`
	MinimumSupported string `json:"minimum_supported"`
	Critical         bool   `json:"critical"`
}

// ServerInfo is a chassis's self-reported build identity plus its client
// policy, read from <base>/healthz?format=json. It doubles as a
// "what's deployed?" surface (Version/Commit/Chassis).
type ServerInfo struct {
	Status         string  `json:"status"`
	Version        string  `json:"version"`
	Commit         string  `json:"commit"`
	Chassis        string  `json:"chassis"`
	BuildTimestamp string  `json:"build_timestamp"`
	Client         *Policy `json:"client"`
}

// FetchServerInfo GETs the JSON form of <baseURL>/healthz (the chassis admin
// base, e.g. https://admin.thanks.computer). Best-effort: callers treat any
// error as "no server policy / unreachable" and carry on.
func FetchServerInfo(ctx context.Context, baseURL, userAgent string) (ServerInfo, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ServerInfo{}, fmt.Errorf("update: empty server base URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz?format=json", nil)
	if err != nil {
		return ServerInfo{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return ServerInfo{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ServerInfo{}, fmt.Errorf("update: GET %s/healthz: %d", base, resp.StatusCode)
	}
	var info ServerInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}

// OutdatedNotice returns a human warning when `current` is older than the
// policy's minimum_supported (louder when Critical), or "" when in sync,
// when the policy is absent, or when current isn't valid semver. This is
// WARN-ONLY — it never signals "block".
func OutdatedNotice(current string, p *Policy) string {
	if p == nil || p.MinimumSupported == "" {
		return ""
	}
	cv, mv := ensureV(current), ensureV(p.MinimumSupported)
	if !semver.IsValid(cv) || !semver.IsValid(mv) {
		return ""
	}
	if semver.Compare(cv, mv) >= 0 {
		return ""
	}
	latest := p.Latest
	if latest == "" {
		latest = p.MinimumSupported
	}
	if p.Critical {
		return fmt.Sprintf("CRITICAL: txco %s is below the server's minimum supported v%s — upgrade now with `txco upgrade` (latest v%s).",
			current, p.MinimumSupported, latest)
	}
	return fmt.Sprintf("warning: txco %s is older than the server's minimum supported v%s — please run `txco upgrade` (latest v%s).",
		current, p.MinimumSupported, latest)
}
