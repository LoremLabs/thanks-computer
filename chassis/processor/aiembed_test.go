package processor

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/embed"
)

func TestDecodeEmbedWith(t *testing.T) {
	// single text
	w, err := decodeEmbedWith(`{"text":"hello","model":"nomic-embed-text","dimensions":256,"provider":"ollama"}`)
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if !w.single || len(w.texts) != 1 || w.texts[0] != "hello" {
		t.Fatalf("single decode wrong: %+v", w)
	}
	if w.model != "nomic-embed-text" || w.dimensions != 256 || w.provider != "ollama" {
		t.Fatalf("fields wrong: %+v", w)
	}

	// batch
	w, err = decodeEmbedWith(`{"texts":["a","b","c"]}`)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if w.single || len(w.texts) != 3 {
		t.Fatalf("batch decode wrong: %+v", w)
	}

	// error cases
	for _, meta := range []string{
		``,                           // empty
		`{"model":"x"}`,              // neither text nor texts
		`{"text":"a","texts":["b"]}`, // both
		`{"texts":"not-an-array"}`,   // texts wrong type
		`{"texts":[]}`,               // empty texts
	} {
		if _, err := decodeEmbedWith(meta); err == nil {
			t.Fatalf("expected error for meta %q", meta)
		}
	}
}

func TestBuildEmbedResponseEnvelopeSingle(t *testing.T) {
	resp := embed.Response{
		Vectors: [][]float32{{0.1, 0.2, 0.3}}, Provider: "ollama",
		Model: "nomic-embed-text", Dimensions: 3, Tokens: 4, LatencyMS: 9,
	}
	raw := buildEmbedResponseEnvelope(resp, nil, "default", true)

	if gjson.Get(raw, "embed.error").Exists() {
		t.Fatalf("unexpected embed.error: %s", raw)
	}
	if n := len(gjson.Get(raw, "_embed.vector").Array()); n != 3 {
		t.Fatalf("_embed.vector len=%d, want 3: %s", n, raw)
	}
	if n := len(gjson.Get(raw, "_embed.vectors").Array()); n != 1 {
		t.Fatalf("_embed.vectors len=%d, want 1: %s", n, raw)
	}
	if got := gjson.Get(raw, "_embed.dimensions").Int(); got != 3 {
		t.Fatalf("_embed.dimensions=%d, want 3", got)
	}
	if got := gjson.Get(raw, "_embed.provider").String(); got != "ollama" {
		t.Fatalf("_embed.provider=%q, want ollama", got)
	}
}

func TestBuildEmbedResponseEnvelopeBatchHasNoSingleVector(t *testing.T) {
	resp := embed.Response{Vectors: [][]float32{{1, 2}, {3, 4}}, Provider: "ollama", Dimensions: 2}
	raw := buildEmbedResponseEnvelope(resp, nil, "default", false)
	if gjson.Get(raw, "_embed.vector").Exists() {
		t.Fatalf("batch must not emit _embed.vector: %s", raw)
	}
	if n := len(gjson.Get(raw, "_embed.vectors").Array()); n != 2 {
		t.Fatalf("_embed.vectors len=%d, want 2", n)
	}
}

func TestBuildEmbedResponseEnvelopeError(t *testing.T) {
	raw := buildEmbedResponseEnvelope(
		embed.Response{Provider: "ollama"},
		&embed.ProviderHTTPError{StatusCode: 500, Body: "boom"},
		"default", true,
	)
	if got := gjson.Get(raw, "embed.error.code").String(); got != "txco_embed_provider_http" {
		t.Fatalf("embed.error.code=%q, want txco_embed_provider_http: %s", got, raw)
	}
	if gjson.Get(raw, "_embed.vectors").Exists() {
		t.Fatalf("error envelope must not carry vectors: %s", raw)
	}
}
