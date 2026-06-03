package admin

import (
	"fmt"
	"net/http"
	"strings"
)

// tracesShowLatest caps the HTML index at this many entries. Trace
// stores can grow large; a latest-N view is enough for a browser
// listing (a navigable archive is a job for the JSON API / a tool).
const tracesShowLatest = 250

// handleTraceRequestsIndex serves a minimal, newest-first listing of
// the most recent traces. The newest-N ids + total come from the
// trace.Reader (filesystem by default; a non-fs backend when admin is
// a separate machine); rendering stays here.
func (c *Controller) handleTraceRequestsIndex(w http.ResponseWriter, r *http.Request) {
	// Flat-only chassis-wide HTML index (operator convenience): super-admin
	// only. traceTenantScope returns "" here (no {tenant} in the path) and
	// requires super-admin.
	if _, ok := c.traceTenantScope(w, r); !ok {
		return
	}
	rdr, rerr := c.traceRdr()
	if rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
		return
	}
	names, total, err := rdr.IndexNames(r.Context(), tracesShowLatest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><title>traces/requests/</title>`)
	b.WriteString(`<style>body{font-family:ui-monospace,monospace;margin:1em}a{color:inherit}.muted{color:#888}</style>`)
	b.WriteString(`<h2>traces / requests /</h2>`)

	if total == 0 {
		b.WriteString(`<p class="muted">-</p>`)
	} else {
		b.WriteString(`<pre>`)
		for _, name := range names {
			fmt.Fprintf(&b, `<a href="%s/">%s/</a>`+"\n", htmlAttr(name), htmlText(name))
		}
		b.WriteString(`</pre>`)
	}

	_, _ = w.Write([]byte(b.String()))
}

// htmlAttr / htmlText are tiny escapers. Request IDs are hxid
// (alphanumerics) so they don't need escaping in practice, but a
// hand-edited store could contain anything; defensive escaping is cheap.
func htmlAttr(s string) string {
	r := strings.NewReplacer(
		`&`, "&amp;",
		`"`, "&quot;",
		`'`, "&#39;",
		`<`, "&lt;",
		`>`, "&gt;",
	)
	return r.Replace(s)
}

func htmlText(s string) string {
	r := strings.NewReplacer(
		`&`, "&amp;",
		`<`, "&lt;",
		`>`, "&gt;",
	)
	return r.Replace(s)
}
