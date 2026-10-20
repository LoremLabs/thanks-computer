package server

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

func callRead(t *testing.T, d *htmlExtractDeps, tenant, meta string) (string, *captureTracer) {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	tr := &captureTracer{}
	ctx = trace.WithContext(ctx, tr)
	ctx = operation.WithMeta(ctx, meta)
	pl, err := htmlRead(ctx, d, []byte(`{"_txc":{"op":"web/3000/read"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw, tr
}

const readPage = `<html><head><title>Read me</title></head><body><nav>menu</nav><main><h1>Title</h1><p>First paragraph with a <a href="/next">link</a>.</p><p>Second.</p></main></body></html>`

func TestHTMLReadAnswers(t *testing.T) {
	f := &stubFetcher{res: page(readPage)}
	out, tr := callRead(t, newHXDeps(f, nil), "acme", `{"url": "https://example.test/a?token=s3cret"}`)
	want := map[string]string{
		"_page.ok":           "true",
		"_page.url":          "https://example.test/a?token=s3cret",
		"_page.final_url":    "https://example.test/b",
		"_page.status":       "200",
		"_page.content_type": "text/html; charset=utf-8",
		"_page.title":        "Read me",
		"_page.root":         "main",
		"_page.part":         "1",
		"_page.parts":        "1",
		"_page.text":         "# Title\n\nFirst paragraph with a [link](https://example.test/next).\n\nSecond.",
	}
	for path, v := range want {
		if got := gjson.Get(out, path).String(); got != v {
			t.Errorf("%s = %q, want %q (%s)", path, got, v, out)
		}
	}
	if gjson.Get(out, "_page.error").Exists() || gjson.Get(out, "_page.truncated").Exists() {
		t.Fatalf("unexpected fields: %s", out)
	}
	if f.urls[0] != "https://example.test/a?token=s3cret" {
		t.Errorf("fetched %q", f.urls[0])
	}
	ev := tr.last()
	if ev.Event != "html.read.completion" || ev.Fields["host"] != "example.test" || ev.Fields["parts"] != 1 || ev.Fields["root"] != "main" {
		t.Errorf("event %+v", ev)
	}
	for k := range ev.Fields {
		if k == "url" || k == "text" || k == "title" {
			t.Errorf("the event carries %s", k)
		}
	}
}

func TestHTMLReadOptions(t *testing.T) {
	f := &stubFetcher{res: page(readPage)}
	d := newHXDeps(f, nil)
	// into, root, part last, part_chars.
	out, _ := callRead(t, d, "acme", `{"url": "https://example.test/a", "into": "_doc", "root": "p", "part": "last", "part_chars": 200}`)
	if gjson.Get(out, "_doc.root").String() != "p" || gjson.Get(out, "_doc.text").String() != "First paragraph with a [link](https://example.test/next)." {
		t.Errorf("%s", out)
	}
	// A part past the last says how many there are.
	out, _ = callRead(t, d, "acme", `{"url": "https://example.test/a", "part": 7}`)
	if gjson.Get(out, "_page.ok").Bool() || gjson.Get(out, "_page.error.code").String() != "txco_html_no_such_part" || gjson.Get(out, "_page.parts").Int() != 1 {
		t.Errorf("%s", out)
	}
	// Malformed fields are refused before any fetch.
	calls := f.calls
	for _, meta := range []string{
		`{"url": "https://example.test/a", "part": 0}`,
		`{"url": "https://example.test/a", "part": "two"}`,
		`{"url": "https://example.test/a", "part": 1.5}`,
		`{"url": "https://example.test/a", "part_chars": 10}`,
		`{"url": "https://example.test/a", "part_chars": "lots"}`,
		`{"url": "https://example.test/a", "root": 3}`,
	} {
		out, _ = callRead(t, d, "acme", meta)
		if code := gjson.Get(out, "_page.error.code").String(); code != "txco_html_invalid_request" {
			t.Errorf("%s → %s: %s", meta, code, out)
		}
	}
	out, _ = callRead(t, d, "acme", `{"url": "https://example.test/a", "root": "p:nth-child(2)"}`)
	if code := gjson.Get(out, "_page.error.code").String(); code != "txco_html_invalid_selectors" {
		t.Errorf("refused selector → %s", code)
	}
	out, _ = callRead(t, d, "acme", `{"url": "https://example.test/a", "root": "#nope"}`)
	if code := gjson.Get(out, "_page.error.code").String(); code != "txco_html_root_not_found" {
		t.Errorf("missing root → %s", code)
	}
	if f.calls != calls+2 {
		t.Errorf("fetched %d times for the refused requests", f.calls-calls)
	}
	// No tenant, a bad URL.
	out, _ = callRead(t, d, "", `{"url": "https://example.test/a"}`)
	if gjson.Get(out, "_page.error.code").String() != "txco_html_no_tenant" {
		t.Errorf("%s", out)
	}
	out, _ = callRead(t, d, "acme", `{"url": "ftp://example.test/a"}`)
	if gjson.Get(out, "_page.error.code").String() != "txco_html_invalid_url" {
		t.Errorf("%s", out)
	}
}

func TestHTMLReadFetchErrors(t *testing.T) {
	f := &stubFetcher{err: &fetch.Error{Code: fetch.CodeHTTPError, Message: "the server answered 404", Status: 404, ContentType: "text/html"}}
	out, _ := callRead(t, newHXDeps(f, nil), "acme", `{"url": "https://example.test/gone"}`)
	if gjson.Get(out, "_page.ok").Bool() || gjson.Get(out, "_page.error.code").String() != "txco_html_http_error" || gjson.Get(out, "_page.error.status").Int() != 404 || gjson.Get(out, "_page.content_type").String() != "text/html" {
		t.Errorf("%s", out)
	}
	f = &stubFetcher{err: &fetch.Error{Code: fetch.CodeTimeout, Message: "the fetch took too long"}}
	out, _ = callRead(t, newHXDeps(f, nil), "acme", `{"url": "https://example.test/slow"}`)
	if gjson.Get(out, "_page.error.code").String() != "txco_html_timeout" {
		t.Errorf("%s", out)
	}
}

func TestHTMLReadSharesTheTenantLimits(t *testing.T) {
	f := &stubFetcher{res: page(readPage), hold: make(chan struct{})}
	d := newHXDeps(f, func(d *htmlExtractDeps) { d.tenantMax = 1 })
	done := make(chan string, 1)
	go func() {
		out, _ := callRead(t, d, "acme", `{"url": "https://example.test/a"}`)
		done <- out
	}()
	for {
		d.mu.Lock()
		n := d.inflight["acme"]
		d.mu.Unlock()
		if n == 1 {
			break
		}
	}
	// A second read, and an extraction, are both busy: one allowance.
	out, _ := callRead(t, d, "acme", `{"url": "https://example.test/a"}`)
	if gjson.Get(out, "_page.error.code").String() != "txco_html_busy" {
		t.Errorf("second read: %s", out)
	}
	out = callHX(t, d, "acme", previewMeta)
	if gjson.Get(out, "_html.error.code").String() != "txco_html_busy" {
		t.Errorf("an extraction beside a read: %s", out)
	}
	close(f.hold)
	if out := <-done; !gjson.Get(out, "_page.ok").Bool() {
		t.Errorf("first read: %s", out)
	}
}

func TestHTMLReadTruncatedPage(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body><main>")
	for i := 0; i < 3000; i++ {
		b.WriteString("<p>words and more words</p>")
	}
	b.WriteString("</main></body></html>")
	f := &stubFetcher{res: page(b.String())}
	d := newHXDeps(f, func(d *htmlExtractDeps) { d.limits.MaxReadBytes = 5000 })
	out, _ := callRead(t, d, "acme", `{"url": "https://example.test/long"}`)
	if !gjson.Get(out, "_page.ok").Bool() || !gjson.Get(out, "_page.truncated").Bool() || gjson.Get(out, "_page.parts").Int() < 2 {
		t.Errorf("%s", out[:200])
	}
}
