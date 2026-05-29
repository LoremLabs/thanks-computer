// Package ui owns the embedded continuation "please wait" Svelte bundle.
//
// Unlike the admin UI this is NOT served from a static route: the chassis
// returns the built single-file index.html as the *body* of the
// continuation poll response (so the browser stays on
// ?_txc.continuation=<rcid> and keeps polling). The bundle is produced by
// `cd continuation-ui && pnpm run build` (vite + vite-plugin-singlefile
// inlines all JS/CSS) and baked in via go:embed at compile time.
package ui

import (
	"embed"
	"io/fs"
)

// `all:` includes dotfiles so dist/.gitkeep alone keeps the directive
// happy before the first build. The real build emits a single
// self-contained dist/index.html beside it.
//
//go:embed all:dist
var distFS embed.FS

// WaitPage returns the page to serve while a continuation is running.
// built is true when the real Svelte bundle is present; false when it
// returns the minimal no-build fallback (pnpm absent / UI not built) so
// the chassis still serves a sane auto-refreshing page.
func WaitPage() (html []byte, built bool) {
	if b, err := fs.ReadFile(distFS, "dist/index.html"); err == nil && len(b) > 0 {
		return b, true
	}
	return []byte(fallbackHTML), false
}

// fallbackHTML is a tiny self-contained, on-brand waiting page used only
// when the Svelte bundle hasn't been built. No JS: a plain meta-refresh
// re-requests the same ?_txc.continuation=<rcid> URL every 3s — the
// chassis serves this until the run completes, then the rendered result
// page (so the loop ends on its own).
const fallbackHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<meta name="robots" content="noindex" />
<meta http-equiv="refresh" content="3" />
<title>working…</title>
<style>
  :root { color-scheme: light; }
  html,body{height:100%;margin:0}
  body{display:flex;align-items:center;justify-content:center;background:#fafafa;color:#0a0a0a;
       font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Monaco,Consolas,monospace}
  .card{width:100%;max-width:28rem;margin:0 1rem;padding:2.5rem;text-align:center;background:#fff;
        border:1px solid #e5e5e5;border-radius:.5rem;box-shadow:0 1px 2px rgba(0,0,0,.05)}
  .mark{font-size:1.5rem;font-weight:600;letter-spacing:-.01em}
  .o1{color:#06b6d4}.o2{color:#ec4899}.o3{color:#fbbf24}
  @keyframes cmy{0%,100%{color:#06b6d4}33%{color:#ec4899}66%{color:#fbbf24}}
  @media (prefers-reduced-motion:no-preference){
    .o1,.o2,.o3{animation:cmy 2.4s steps(1,end) infinite}
    .o2{animation-delay:-.8s}.o3{animation-delay:-1.6s}
  }
  .msg{margin-top:1.5rem;font-size:.875rem;color:#525252}
  .sub{margin-top:.5rem;font-size:.75rem;color:#a3a3a3}
</style>
</head>
<body>
  <div class="card">
    <div class="mark">thanks, c<span class="o1">o</span><span class="o2">o</span><span class="o3">o</span>mputer.</div>
    <p class="msg">working…</p>
    <p class="sub">This page updates automatically.</p>
  </div>
</body>
</html>`
