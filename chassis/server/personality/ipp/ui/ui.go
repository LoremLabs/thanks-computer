// Package ui owns the embedded printer page: what a browser sees at a
// printer's own address (https://ipp.<zone>/p/<printer> — the https twin of
// its ipps:// URI, and where a configuration profile's description points).
//
// Like the continuation page it is NOT served from a static route: the ipp
// head returns it for a GET on a printer that exists, with the printer's
// details injected at the <!--txco:printer--> marker. The bundle is produced
// by `cd printer-ui && pnpm run build` (vite + vite-plugin-singlefile inlines
// all JS/CSS) and baked in via go:embed at compile time. It is an information
// page and nothing more — no credential, no download: what a product builds
// around its printers (a setup profile, a name) belongs to the product.
package ui

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
)

// `all:` includes dotfiles so dist/.gitkeep alone keeps the directive
// happy before the first build. The real build emits a single
// self-contained dist/index.html beside it.
//
//go:embed all:dist
var distFS embed.FS

// marker is where the built page expects the printer's details.
const marker = "<!--txco:printer-->"

// Printer is what the page shows: the fields macOS's Add Printer › IP tab
// asks for, plus the address a client adds the printer by.
type Printer struct {
	Name    string `json:"name"`    // the registered display name, else the label
	URI     string `json:"uri"`     // ipps://ipp.<zone>:443/p/<printer>
	Address string `json:"address"` // ipp.<zone>:443 — the port always spelled out
	Queue   string `json:"queue"`   // p/<printer>, or p/<handle>/<printer>
}

// Page returns the printer page. built is true when the real Svelte bundle
// is present; false when it returns the no-build fallback (pnpm absent / UI
// not built), which says the same things without JavaScript.
func Page(p Printer) (html []byte, built bool) {
	if b, err := fs.ReadFile(distFS, "dist/index.html"); err == nil && bytes.Contains(b, []byte(marker)) {
		// json.Marshal escapes <, > and & (and U+2028/9), so the object
		// cannot close the <script> it is placed in.
		data, err := json.Marshal(p)
		if err == nil {
			script := `<script id="txco-printer" type="application/json">` + string(data) + `</script>`
			return bytes.Replace(b, []byte(marker), []byte(script), 1), true
		}
	}
	return fallbackPage(p), false
}

// fallbackPage renders the no-build page.
func fallbackPage(p Printer) []byte {
	// html/template rewrites a URL whose scheme it does not know (ipps:) to
	// #ZgotmplZ. The head builds this one from a validated host and printer
	// label, so it is marked trusted for the one attribute that links it.
	view := struct {
		Printer
		Href template.URL
	}{p, template.URL(p.URI)}
	var buf bytes.Buffer
	if err := fallback.Execute(&buf, view); err != nil {
		return []byte("printer: " + template.HTMLEscapeString(p.URI))
	}
	return buf.Bytes()
}

// fallback is the page without the Svelte bundle: the same card, rendered
// on the server, no JavaScript.
var fallback = template.Must(template.New("printer").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1.0" />
<meta name="robots" content="noindex" />
<title>{{.Name}} · virtual printer</title>
<style>
  :root { color-scheme: light; }
  html,body{height:100%;margin:0}
  body{display:flex;align-items:center;justify-content:center;background:#fafafa;color:#0a0a0a;
       font-family:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Monaco,Consolas,monospace}
  .card{width:100%;max-width:28rem;margin:2.5rem 1rem;padding:2rem;background:#fff;
        border:1px solid #e5e5e5;border-radius:.5rem;box-shadow:0 1px 2px rgba(0,0,0,.05)}
  .mark{display:block;font-size:1.5rem;font-weight:600;letter-spacing:-.01em;text-align:center;color:#171717;text-decoration:none}
  .o1{color:#06b6d4}.o2{color:#ec4899}.o3{color:#fbbf24}
  .label{margin-top:2rem;font-size:.75rem;color:#a3a3a3;text-transform:uppercase;letter-spacing:.05em}
  h1{margin:.25rem 0 0;font-size:1.25rem;word-break:break-all}
  p{font-size:.875rem;color:#525252}
  a.add{display:block;margin-top:1.5rem;padding:.75rem 1rem;border-radius:.375rem;background:#171717;
        color:#fff;text-align:center;font-size:.875rem;font-weight:600;text-decoration:none}
  dl{display:grid;grid-template-columns:auto 1fr;gap:.5rem 1rem;font-size:.875rem}
  dt{color:#737373} dd{margin:0;text-align:right;word-break:break-all}
  .gap{height:3rem}
  code{display:block;margin-top:.5rem;padding:.5rem .75rem;border:1px solid #e5e5e5;border-radius:.375rem;
       background:#fafafa;font-size:.75rem;word-break:break-all}
</style>
</head>
<body>
  <div class="card">
    <a class="mark" href="https://www.thanks.computer/?utm_source=printer_setup">thanks, c<span class="o1">o</span><span class="o2">o</span><span class="o3">o</span>mputer.</a>
    <div class="label">virtual printer</div>
    <h1>{{.Name}}</h1>
    <div class="gap"></div>
    <a class="add" href="{{.Href}}">Add printer</a>
    <div class="label">Manual Setup</div>
    <dl>
      <dt>Address</dt><dd>{{.Address}}</dd>
      <dt>Protocol</dt><dd>IPP (Internet Printing Protocol)</dd>
      <dt>Queue</dt><dd>{{.Queue}}</dd>
    </dl>
    <p>On a Mac: System Settings › Printers &amp; Scanners › Add Printer › IP. Elsewhere, add a printer by its address:</p>
    <code>{{.URI}}</code>
  </div>
</body>
</html>`))
