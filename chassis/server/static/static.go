// Package static serves bundled / workspace files for the txco://static
// op, layered first-match-wins: a routed stack's own
// <workspace>/OPS/<stack>/FILES/** , the chassis-wide
// <workspace>/FILES/** , then the embedded chassis default
// (favicon.ico).
//
// The set + the bytes are loaded into memory at startup and rebuilt on
// dbcache reload (see Index). A request NEVER touches the filesystem:
// Resolve is a pure in-memory radix lookup, so static serving and the
// boot pipeline that calls it stay off the disk critical path.
package static

import (
	"embed"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

// MaxFileBytes caps any single static file. Same 1 MiB ceiling the
// continuation worker callback uses (server/personality/web/continuation.go).
// Larger files are skipped at index-build time (never read per request).
const MaxFileBytes = 1 << 20

// Index-build budget across the workspace layers (the embedded default
// is trusted and always loaded, exempt from these). Bounds the
// in-memory cache so a runaway FILES/ tree can't exhaust RAM; overflow
// is logged and skipped (already-indexed files still serve).
const (
	MaxIndexFiles = 2048
	MaxIndexBytes = 64 << 20
	MaxIndexDepth = 10
)

// all: keeps go:embed from skipping dotfiles and guarantees the
// directory is non-empty (favicon.ico ships by default).
//
//go:embed all:files
var embeddedFS embed.FS

// byExt pins content-types for the common static/web/text extensions.
// It is authoritative on purpose: mime.TypeByExtension augments Go's
// minimal builtin table from the OS mime database, which is absent or
// stripped in many containers — there .css/.js/.md would silently
// become application/octet-stream. Pinning makes the result
// deterministic regardless of deploy environment; mime is only a
// fallback for the long tail.
var byExt = map[string]string{
	// markup / text
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".cjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".map":         "application/json",
	".xml":         "application/xml",
	".txt":         "text/plain; charset=utf-8",
	".md":          "text/markdown; charset=utf-8",
	".csv":         "text/csv; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	// images
	".ico":  "image/x-icon",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	// fonts
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".eot":   "application/vnd.ms-fontobject",
}

// contentType resolves a file's content-type: the pinned byExt table
// first (deterministic across environments), then the OS-augmented mime
// table, then content sniffing of the bytes (extension-less files,
// odd extensions). content is the file's bytes — already in memory at
// index-build time, so the sniff costs nothing on the request path.
// http.DetectContentType never returns "" (it defaults to
// application/octet-stream itself).
func contentType(name string, content []byte) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := byExt[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	if len(content) > 0 {
		return http.DetectContentType(content)
	}
	return "application/octet-stream"
}

// safeRel normalizes a request path to a clean, traversal-safe slash
// relative path ("" = reject). Rooting through path.Clean collapses
// "..", and every segment is re-checked (no "", ".", "..", no
// dot-prefixed names → no dotfiles/dotdirs). Used both at index-build
// (the walk is trusted, this is defense in depth) and per request.
func safeRel(p string) string {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		return ""
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || seg[0] == '.' {
			return ""
		}
	}
	return p
}

// safeSeg validates a single path segment (the routed stack name).
func safeSeg(s string) string {
	if s == "" || s == "." || s == ".." || strings.ContainsRune(s, '/') || s[0] == '.' {
		return ""
	}
	return s
}
