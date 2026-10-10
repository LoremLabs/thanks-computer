package web

import (
	"mime"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyNegotiation renders detect-tenant's markdown-negotiation marks
// (`_txc.web.negotiated`, set only on a host whose stack has a
// `_markdown` inlet) as response headers:
//
//   - vary: added to Vary unless a value already names it (or `*`), on
//     every answer, static or op, HTML or markdown;
//   - alternate: a `Link: <…>; rel="alternate"; type="text/markdown"` on
//     an HTML answer only, appended to any Link the stack set.
//
// No marks, no change: a site without the inlet never sends either. Called
// after checkContentType, so the answer's media type is settled.
func applyNegotiation(output string) string {
	n := gjson.Get(output, "_txc.web.negotiated")
	if !n.Exists() {
		return output
	}
	if v := n.Get("vary").String(); v != "" && !headerListHas(output, "Vary", v) {
		output = appendResHeader(output, "Vary", v)
	}
	if alt := n.Get("alternate").String(); alt != "" && isHTMLAnswer(output) {
		output = appendResHeader(output, "Link", "<"+alt+`>; rel="alternate"; type="text/markdown"`)
	}
	return output
}

// resHeaderKey returns the key the envelope holds a header under, matched
// the way writeResHeaders canonicalizes it, or "" when there is none.
func resHeaderKey(output, name string) string {
	want := http.CanonicalHeaderKey(name)
	found := ""
	gjson.Get(output, "_txc.web.res.headers").ForEach(func(k, _ gjson.Result) bool {
		if http.CanonicalHeaderKey(k.String()) == want {
			found = k.String()
			return false
		}
		return true
	})
	return found
}

// headerListHas reports whether a list-valued header already names token
// (case-insensitively), or is `*`.
func headerListHas(output, name, token string) bool {
	key := resHeaderKey(output, name)
	if key == "" {
		return false
	}
	has := false
	gjson.Get(output, "_txc.web.res.headers").Get(gjson.Escape(key)).ForEach(func(_, v gjson.Result) bool {
		for _, t := range strings.Split(v.String(), ",") {
			t = strings.TrimSpace(t)
			if t == "*" || strings.EqualFold(t, token) {
				has = true
				return false
			}
		}
		return true
	})
	return has
}

// appendResHeader adds one value to a header, under the key the envelope
// already uses for it (an array, or a lone string made one), else a new
// lowercase key.
func appendResHeader(output, name, value string) string {
	key := resHeaderKey(output, name)
	if key == "" {
		out, err := sjson.Set(output, "_txc.web.res.headers."+strings.ToLower(name)+".0", value)
		if err != nil {
			return output
		}
		return out
	}
	path := "_txc.web.res.headers." + gjson.Escape(key)
	cur := gjson.Get(output, path)
	var vals []string
	if cur.IsArray() {
		cur.ForEach(func(_, v gjson.Result) bool {
			vals = append(vals, v.String())
			return true
		})
	} else if cur.Exists() {
		vals = append(vals, cur.String())
	}
	out, err := sjson.Set(output, path, append(vals, value))
	if err != nil {
		return output
	}
	return out
}

// isHTMLAnswer reports whether the answer's Content-Type is text/html.
func isHTMLAnswer(output string) bool {
	key := resHeaderKey(output, "Content-Type")
	if key == "" {
		return false
	}
	ct := gjson.Get(output, "_txc.web.res.headers").Get(gjson.Escape(key))
	if ct.IsArray() {
		ct = ct.Get("0")
	}
	mt, _, err := mime.ParseMediaType(ct.String())
	return err == nil && mt == "text/html"
}
