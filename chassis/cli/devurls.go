package cli

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
)

// devURLs is the stack→URL handoff between `txco dev` and `txco status`.
//
// The dev chassis mints a random per-stack structured hostname
// (`<stack>-<rand>.localhost`) that's otherwise hard to discover, and
// `txco status` targets the *remote* (prod) chassis, so it can't see the
// local one. `txco dev` resolves each stack's reachable dev URL after apply
// and writes it here; `txco status` reads it to surface the dev URL
// alongside the remote one. Lives under the gitignored .txco/dev/ tree.

func devURLsPath(dir string) string {
	return filepath.Join(dir, ".txco", "dev", "urls.json")
}

// writeDevURLs persists the stack→URL map (best-effort; the caller logs).
func writeDevURLs(dir string, urls map[string]string) error {
	p := devURLsPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(urls, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// readDevURLs loads the stack→URL map; a missing/unreadable file yields an
// empty map (the dev URL is simply unknown).
func readDevURLs(dir string) map[string]string {
	b, err := os.ReadFile(devURLsPath(dir))
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	_ = json.Unmarshal(b, &m)
	return m
}

// devStackURL builds a stack's dev URL from the dev web base (e.g.
// "http://localhost:8080") and its structured hostname: keep the base's
// scheme + port, swap in the hostname. Empty base/host → "".
func devStackURL(webBase, host string) string {
	if webBase == "" || host == "" {
		return ""
	}
	u, err := url.Parse(webBase)
	if err != nil || u.Scheme == "" {
		return ""
	}
	h := host
	if p := u.Port(); p != "" {
		h = host + ":" + p
	}
	return u.Scheme + "://" + h
}
