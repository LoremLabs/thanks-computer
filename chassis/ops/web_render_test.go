package ops

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// decodeBody pulls the base64-encoded _txc.web.res.body out of a
// WebRender response and returns the raw bytes — most tests want to
// assert on the rendered body, not its base64 wrapper.
func decodeBody(t *testing.T, raw string) string {
	t.Helper()
	field := gjson.Get(raw, "_txc.web.res.body")
	if !field.Exists() {
		t.Fatalf("response missing _txc.web.res.body: %s", raw)
	}
	b64 := field.String()
	if b64 == "" {
		// Field is set but empty — legitimate "no body" result.
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v (raw=%q)", err, b64)
	}
	return string(decoded)
}

func TestWebRenderDefaultsToRawText(t *testing.T) {
	in := []byte(`{"text":"hello world"}`)
	out, err := WebRender(withMeta(`{}`), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	if body := decodeBody(t, out.Raw); body != "hello world" {
		t.Errorf("body = %q, want %q", body, "hello world")
	}
	if ct := gjson.Get(out.Raw, "_txc.web.res.headers.content-type.0").String(); ct != "text/plain; charset=utf-8" {
		t.Errorf("default content-type = %q, want text/plain", ct)
	}
	if st := gjson.Get(out.Raw, "_txc.web.res.status").Int(); st != 200 {
		t.Errorf("default status = %d, want 200", st)
	}
	if !gjson.Get(out.Raw, "_txc.halt").Bool() {
		t.Errorf("halt not set on response")
	}
}

func TestWebRenderHTMLWrapEscapes(t *testing.T) {
	// HTML wrap must escape source characters so any stray markup
	// in the content is rendered literally (security: an MCP server
	// could return `<script>` in its text and we don't want that to
	// execute when the operator opts for "html" wrap).
	in := []byte(`{"text":"<script>alert('xss')</script>"}`)
	out, err := WebRender(withMeta(`{"wrap":"html"}`), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	body := decodeBody(t, out.Raw)
	if strings.Contains(body, "<script>") {
		t.Errorf("unescaped <script> in html wrap: %q", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected escaped <script>, got %q", body)
	}
	if ct := gjson.Get(out.Raw, "_txc.web.res.headers.content-type.0").String(); ct != "text/html; charset=utf-8" {
		t.Errorf("html-wrap content-type = %q, want text/html", ct)
	}
}

func TestWebRenderMarkdownToHTML(t *testing.T) {
	in := []byte(`{"text":"# Hello\n\n**bold** and a [link](https://example.com)."}`)
	out, err := WebRender(withMeta(`{"wrap":"markdown-to-html"}`), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	body := decodeBody(t, out.Raw)
	for _, want := range []string{"<h1>Hello</h1>", "<strong>bold</strong>", `href="https://example.com"`} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered HTML missing %q\n--- body ---\n%s", want, body)
		}
	}
	if ct := gjson.Get(out.Raw, "_txc.web.res.headers.content-type.0").String(); ct != "text/html; charset=utf-8" {
		t.Errorf("md-to-html content-type = %q, want text/html", ct)
	}
}

func TestWebRenderCustomSource(t *testing.T) {
	// `source` can name any envelope path, not just `.text`.
	in := []byte(`{"_txc":{"computed":{"answer":"from-elsewhere"}}}`)
	out, err := WebRender(withMeta(`{"source":"_txc.computed.answer"}`), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	if body := decodeBody(t, out.Raw); body != "from-elsewhere" {
		t.Errorf("custom-source body = %q, want from-elsewhere", body)
	}
}

func TestWebRenderCustomContentTypeAndStatus(t *testing.T) {
	in := []byte(`{"text":"plain bytes"}`)
	meta := `{"content_type":"application/json","status":418}`
	out, err := WebRender(withMeta(meta), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	if ct := gjson.Get(out.Raw, "_txc.web.res.headers.content-type.0").String(); ct != "application/json" {
		t.Errorf("custom content-type = %q, want application/json", ct)
	}
	if st := gjson.Get(out.Raw, "_txc.web.res.status").Int(); st != 418 {
		t.Errorf("custom status = %d, want 418", st)
	}
}

func TestWebRenderRejectsUnknownWrap(t *testing.T) {
	in := []byte(`{"text":"x"}`)
	_, err := WebRender(withMeta(`{"wrap":"xml-emit-cthulhu"}`), "txco://web-render", in, nil)
	if err == nil {
		t.Fatal("WebRender with unknown wrap returned nil error")
	}
	if !strings.Contains(err.Error(), "xml-emit-cthulhu") {
		t.Errorf("err = %v, want one naming the bad wrap", err)
	}
}

func TestWebRenderMissingSourceProducesEmptyBody(t *testing.T) {
	// Source path absent — we render an empty body. Authors gate
	// the "not found" alternative with a WHEN on the caller side
	// rather than making this op return an error.
	in := []byte(`{}`)
	out, err := WebRender(withMeta(`{}`), "txco://web-render", in, nil)
	if err != nil {
		t.Fatalf("WebRender: %v", err)
	}
	if body := decodeBody(t, out.Raw); body != "" {
		t.Errorf("missing-source body = %q, want \"\"", body)
	}
	if !gjson.Get(out.Raw, "_txc.halt").Bool() {
		t.Errorf("halt should still be set even on empty body")
	}
}
