package jsonx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/sjson"
)

// Side-by-side cost of the converted call-site shapes: the old
// sjson-chain form vs the Builder. Shapes mirror web.go's request
// envelope (headers + base64 body), readfile.go's per-file accumulate
// loop, and lmtp's attachment array.

var benchSink string

func webShapeOps(chain bool) string {
	headers := map[string][]string{}
	for i := 0; i < 20; i++ {
		headers[fmt.Sprintf("X-Header-%02d", i)] = []string{strings.Repeat("v", 40)}
	}
	body := strings.Repeat("eyJrZXkiOiJ2YWx1ZSJ9", 1000) // ~20KB
	if chain {
		payload, _ := sjson.Set("", "_txc.src", "http")
		payload, _ = sjson.Set(payload, "_txc.rid", "CcxRid123")
		payload, _ = sjson.Set(payload, "_txc.web.req.headers", headers)
		payload, _ = sjson.Set(payload, "_txc.web.req.host", "www.dripl.it")
		payload, _ = sjson.Set(payload, "_txc.web.req.proto", "HTTP/1.1")
		payload, _ = sjson.Set(payload, "_txc.web.req.method", "POST")
		payload, _ = sjson.Set(payload, "_txc.web.req.cookies", map[string][]any{"dcoh": {"202628"}})
		payload, _ = sjson.Set(payload, "_txc.web.req.url.full", "/pub?slug=x")
		payload, _ = sjson.Set(payload, "_txc.web.req.url.path", "/pub")
		payload, _ = sjson.Set(payload, "_txc.web.req.url.query", map[string][]string{"slug": {"x"}})
		payload, _ = sjson.Set(payload, "_txc.web.req.url.query.raw", "slug=x")
		payload, _ = sjson.Set(payload, "_txc.web.req.body", body)
		payload, _ = sjson.Set(payload, "_ts", "2026-07-12T20:00:00Z")
		return payload
	}
	b := New()
	b.Set("_txc.src", "http")
	b.Set("_txc.rid", "CcxRid123")
	b.Set("_txc.web.req.headers", headers)
	b.Set("_txc.web.req.host", "www.dripl.it")
	b.Set("_txc.web.req.proto", "HTTP/1.1")
	b.Set("_txc.web.req.method", "POST")
	b.Set("_txc.web.req.cookies", map[string][]any{"dcoh": {"202628"}})
	b.Set("_txc.web.req.url.full", "/pub?slug=x")
	b.Set("_txc.web.req.url.path", "/pub")
	b.Set("_txc.web.req.url.query", map[string][]string{"slug": {"x"}})
	b.Set("_txc.web.req.url.query.raw", "slug=x")
	b.Set("_txc.web.req.body", body)
	b.Set("_ts", "2026-07-12T20:00:00Z")
	return b.String()
}

func BenchmarkWebEnvelopeShape(b *testing.B) {
	if webShapeOps(true) != webShapeOps(false) {
		b.Fatal("shape mismatch")
	}
	b.Run("sjson", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = webShapeOps(true)
		}
	})
	b.Run("jsonx", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = webShapeOps(false)
		}
	})
}

func readfileShapeOps(chain bool, content string) string {
	if chain {
		resp := `{}`
		for i := 0; i < 5; i++ {
			base := "_files.file" + string(rune('a'+i))
			resp, _ = sjson.Set(resp, base+".found", true)
			resp, _ = sjson.Set(resp, base+".path", "assets/doc.bin")
			resp, _ = sjson.Set(resp, base+".encoding", "base64")
			resp, _ = sjson.Set(resp, base+".ctype", "application/octet-stream")
			resp, _ = sjson.Set(resp, base+".size", len(content))
			resp, _ = sjson.Set(resp, base+".content", content)
		}
		return resp
	}
	b := NewObject()
	for i := 0; i < 5; i++ {
		base := "_files.file" + string(rune('a'+i))
		b.Set(base+".found", true)
		b.Set(base+".path", "assets/doc.bin")
		b.Set(base+".encoding", "base64")
		b.Set(base+".ctype", "application/octet-stream")
		b.Set(base+".size", len(content))
		b.Set(base+".content", content)
	}
	return b.String()
}

func BenchmarkReadFileShape(b *testing.B) {
	content := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 2800) // ~100KB
	if readfileShapeOps(true, content) != readfileShapeOps(false, content) {
		b.Fatal("shape mismatch")
	}
	b.Run("sjson", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = readfileShapeOps(true, content)
		}
	})
	b.Run("jsonx", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = readfileShapeOps(false, content)
		}
	})
}

func lmtpShapeOps(chain bool, content string) string {
	entry := func(i int) map[string]any {
		return map[string]any{
			"name":    fmt.Sprintf("att-%d.pdf", i),
			"type":    "application/pdf",
			"size":    len(content),
			"sha256":  "abcdef0123456789",
			"content": content,
		}
	}
	if chain {
		out := "[]"
		for i := 0; i < 2; i++ {
			out, _ = sjson.Set(out, "-1", entry(i))
		}
		return out
	}
	b := NewArray()
	for i := 0; i < 2; i++ {
		b.Set("-1", entry(i))
	}
	return b.String()
}

func BenchmarkLMTPAttachmentShape(b *testing.B) {
	content := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 5600) // ~200KB
	if lmtpShapeOps(true, content) != lmtpShapeOps(false, content) {
		b.Fatal("shape mismatch")
	}
	b.Run("sjson", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = lmtpShapeOps(true, content)
		}
	})
	b.Run("jsonx", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchSink = lmtpShapeOps(false, content)
		}
	})
}
