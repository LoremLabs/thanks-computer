package processor

import (
	"fmt"
	"strings"
	"testing"
)

// Benchmark doc shapes mirror real envelope traffic: a small web request
// envelope, a large envelope carrying base64 FILES content, a large op
// output (read-file style) merging into a small envelope, and a wide
// many-key merge. Sizes are fixed so before/after runs compare with
// benchstat.

func benchSmallEnvelope() string {
	pu := &Unit{}
	env := `{}`
	env, _ = pu.MergeJSON(env, `{"_txc":{"web":{"req":{"host":"www.example.com","method":"GET","proto":"HTTP/1.1","url":{"full":"/pub?slug=x","path":"/pub","query":{"raw":"slug=x","slug":["x"]}},"headers":{"Accept":["*/*"],"User-Agent":["bench"]},"cookies":{"dcoh":["202628"]}}},"tenant":"bench","stack":"www","route":{"to":"www/0","tenant":"bench","stack":"www"}},"_ts":1783875022}`)
	return env
}

func benchBigEnvelope() string {
	// ~100KB of base64-ish FILES content plus a 20KB request body.
	content := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ejAxMjM0NTY3ODkrLw==", 1200)
	body := strings.Repeat("eyJrZXkiOiJ2YWx1ZSJ9", 1000)
	pu := &Unit{}
	env := benchSmallEnvelope()
	env, _ = pu.MergeJSON(env, fmt.Sprintf(`{"_files":{"welcome":{"found":true,"content":%q,"encoding":"base64","size":102400}},"_txc_body":%q}`, content, body))
	return env
}

func benchManyKeyDoc(n int, prefix string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"%s_%03d":{"v":%d,"s":"value-%d"}`, prefix, i, i, i)
	}
	b.WriteByte('}')
	return b.String()
}

var benchSink string

func BenchmarkMergeSmallEnvelopeSmallOutput(b *testing.B) {
	pu := &Unit{}
	src := benchSmallEnvelope()
	dst := `{"found":true,"status":"ok","count":42}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink, _ = pu.MergeJSON(src, dst)
	}
}

func BenchmarkMergeBigEnvelopeSmallOutput(b *testing.B) {
	pu := &Unit{}
	src := benchBigEnvelope()
	dst := `{"found":true,"status":"ok","count":42,"meta":{"source":"kv","hits":3}}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink, _ = pu.MergeJSON(src, dst)
	}
}

func BenchmarkMergeBigOutputIntoEnvelope(b *testing.B) {
	pu := &Unit{}
	src := benchSmallEnvelope()
	content := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ejAxMjM0NTY3ODkrLw==", 1200)
	dst := fmt.Sprintf(`{"_files":{"a":{"found":true,"content":%q},"b":{"found":true,"content":%q}}}`, content, content)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink, _ = pu.MergeJSON(src, dst)
	}
}

func BenchmarkMergeManyKeys(b *testing.B) {
	pu := &Unit{}
	src := benchManyKeyDoc(200, "src")
	dst := benchManyKeyDoc(50, "dst")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink, _ = pu.MergeJSON(src, dst)
	}
}
