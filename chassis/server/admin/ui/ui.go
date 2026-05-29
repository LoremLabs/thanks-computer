// Package ui owns the embedded admin-ui Svelte bundle and serves it
// over HTTP. The bundle is produced by `cd admin-ui && npm run build`
// (Vite writes into ./dist) and baked into the chassis binary via
// go:embed at compile time.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// `all:` includes dotfiles so dist/.gitkeep alone keeps the directive
// happy before the first `npm run build`. Real build output (index.html
// + assets/) lands beside it.
//
//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded Svelte
// bundle under mountPath (e.g. "/admin"). Unknown paths under the
// mount fall back to index.html so client-side routing survives
// a hard refresh. If the bundle hasn't been built yet (dist/ has
// only .gitkeep), all paths fall back to a small placeholder page
// instructing the operator to run `npm run build`.
func Handler(mountPath string) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.HandlerFunc(servePlaceholder)
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(servePlaceholder)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix(mountPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
			return
		}
		if _, err := fs.Stat(sub, path); err != nil {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	}))
}

const placeholderHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>thanks-computer admin UI (not built)</title>
  <style>
    body { font: 14px/1.5 system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem; color: #222; }
    code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    pre { background: #f3f3f3; padding: 0.75rem 1rem; border-radius: 6px; overflow: auto; }
    h1 { font-size: 1.25rem; }
  </style>
</head>
<body>
  <h1>admin UI bundle not built</h1>
  <p>The embedded Svelte SPA hasn't been built yet. From the repo root:</p>
  <pre>cd admin-ui
npm install
npm run build</pre>
  <p>Then rebuild the chassis binary (<code>go build ./cmd/txco</code>) and reload.</p>
</body>
</html>`

func servePlaceholder(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(placeholderHTML))
}
