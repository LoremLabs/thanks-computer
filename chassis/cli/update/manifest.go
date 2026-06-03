package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The canonical release source. The GitHub Releases API `latest` endpoint
// returns the most recent NON-prerelease release, which matches the release
// workflow's rule that rc tags publish a prerelease and don't move the tap.
const (
	releaseOwner = "loremlabs"
	releaseRepo  = "thanks-computer"
)

// githubAPIBase is the GitHub API root; a var (not a const) so tests can
// point it at an httptest server.
var githubAPIBase = "https://api.github.com"

// Release is the subset of a GitHub release we consume.
type Release struct {
	TagName string  `json:"tag_name"`
	HTMLURL string  `json:"html_url"`
	Assets  []Asset `json:"assets"`
}

// Asset is one downloadable file attached to a release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

// AssetURL returns the browser_download_url for the named asset, or "".
func (r Release) AssetURL(name string) string {
	for _, a := range r.Assets {
		if a.Name == name {
			return a.DownloadURL
		}
	}
	return ""
}

// LatestRelease fetches the latest non-prerelease release from GitHub.
// userAgent should carry the CLI version (e.g. "txco-cli/0.2.3"). The call
// is unauthenticated (GitHub allows 60 req/hr/IP — ample for occasional
// manual checks).
func LatestRelease(ctx context.Context, userAgent string) (Release, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/releases/latest", githubAPIBase, releaseOwner, releaseRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return Release{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Release{}, fmt.Errorf("github releases/latest: %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("github releases/latest: decode: %w", err)
	}
	if rel.TagName == "" {
		return Release{}, fmt.Errorf("github releases/latest: empty tag_name")
	}
	return rel, nil
}
