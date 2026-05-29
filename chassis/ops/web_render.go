package ops

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/yuin/goldmark"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// WebRender is the handler for `txco://web-render`. It reads a value
// from a source envelope path, optionally transforms it (raw / HTML
// wrap / markdown-to-HTML), base64-encodes it, and writes the result
// into the web-response shape `_txc.web.res.*` plus `_txc.halt =
// true`. Designed for the "scope 200 renders the answer that scope
// 100 produced" pattern.
//
// WITH parameters (op.Meta):
//
//	source        = ".text"                          (optional, default ".text")
//	content_type  = "text/plain; charset=utf-8"      (optional, default
//	                                                  matches the wrap mode)
//	status        = 200                              (optional, default 200)
//	wrap          = "raw"                            (optional: "raw" |
//	                                                  "html" | "markdown-to-html")
//
// `wrap` modes:
//
//   - "raw"               — verbatim bytes. Default content-type
//                           "text/plain; charset=utf-8".
//   - "html"              — wrap the (HTML-escaped) source in a
//                           minimal `<pre>` document. Default
//                           content-type "text/html; charset=utf-8".
//   - "markdown-to-html"  — render the source as CommonMark via
//                           goldmark. Default content-type
//                           "text/html; charset=utf-8".
func WebRender(ctx context.Context, _ string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))

	source := gjson.GetBytes(meta, "source").String()
	if source == "" {
		source = "text"
	} else {
		// Accept `@web.req.…` / `.text` / `text` interchangeably,
		// matching txcl's WHEN-path conventions.
		source = normalizePath(source)
	}
	wrap := gjson.GetBytes(meta, "wrap").String()
	if wrap == "" {
		wrap = "raw"
	}
	contentType := gjson.GetBytes(meta, "content_type").String()
	if contentType == "" {
		switch wrap {
		case "html", "markdown-to-html":
			contentType = "text/html; charset=utf-8"
		default:
			contentType = "text/plain; charset=utf-8"
		}
	}
	status := int(gjson.GetBytes(meta, "status").Int())
	if status == 0 {
		status = 200
	}

	srcValue := gjson.GetBytes(in, source).String()

	var bodyBytes []byte
	switch wrap {
	case "raw":
		bodyBytes = []byte(srcValue)
	case "html":
		// Minimal viable HTML document; escape the source so any
		// stray HTML in the content (e.g. markdown rendered to a
		// `<` literal) is shown literally rather than interpreted.
		bodyBytes = []byte("<!doctype html><html><body><pre>" +
			html.EscapeString(srcValue) +
			"</pre></body></html>")
	case "markdown-to-html":
		var buf bytes.Buffer
		if err := goldmark.Convert([]byte(srcValue), &buf); err != nil {
			return errPayload(fmt.Sprintf("web-render: markdown convert: %v", err)),
				fmt.Errorf("web-render: markdown convert: %w", err)
		}
		// Wrap the rendered fragment in a minimal HTML doc so the
		// browser has structure to hang on (otherwise it's an
		// orphan body fragment). Operators wanting a richer
		// template can run the chassis's static serving instead.
		bodyBytes = append([]byte("<!doctype html><html><body>"), buf.Bytes()...)
		bodyBytes = append(bodyBytes, []byte("</body></html>")...)
	default:
		return errPayload(fmt.Sprintf("web-render: unsupported wrap %q (raw|html|markdown-to-html)", wrap)),
			fmt.Errorf("web-render: unsupported wrap %q", wrap)
	}

	b64 := base64.StdEncoding.EncodeToString(bodyBytes)

	resp := `{}`
	resp, _ = sjson.Set(resp, "_txc.web.res.status", status)
	resp, _ = sjson.Set(resp, "_txc.web.res.headers.content-type.0", contentType)
	resp, _ = sjson.Set(resp, "_txc.web.res.body", b64)
	resp, _ = sjson.Set(resp, "_txc.halt", true)

	return event.Payload{Raw: resp, Type: event.JSON}, nil
}
