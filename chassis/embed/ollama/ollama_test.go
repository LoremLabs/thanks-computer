package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/embed"
)

func TestParseOllamaEmbed(t *testing.T) {
	body := []byte(`{"model":"nomic-embed-text","embeddings":[[0.1,0.2,0.3],[0.4,0.5,0.6]],"prompt_eval_count":7}`)
	resp, err := parseOllamaEmbed(body, "ollama", "nomic-embed-text", 12, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("vectors=%d, want 2", len(resp.Vectors))
	}
	if resp.Dimensions != 3 {
		t.Fatalf("dimensions=%d, want 3", resp.Dimensions)
	}
	if resp.Tokens != 7 {
		t.Fatalf("tokens=%d, want 7", resp.Tokens)
	}
	if resp.Vectors[1][2] != 0.6 {
		t.Fatalf("vectors[1][2]=%v, want 0.6", resp.Vectors[1][2])
	}
}

func TestParseOllamaEmbedEmptyAndMalformed(t *testing.T) {
	if _, err := parseOllamaEmbed(nil, "ollama", "m", 0, 0); err == nil {
		t.Fatal("empty body: want error")
	}
	if _, err := parseOllamaEmbed([]byte(`{"embeddings":[]}`), "ollama", "m", 0, 0); err == nil {
		t.Fatal("no embeddings: want error")
	}
	if _, err := parseOllamaEmbed([]byte(`not json`), "ollama", "m", 0, 0); err == nil {
		t.Fatal("malformed: want error")
	}
}

// newTestBackend builds the registered ollama backend pointed at a test server.
func newTestBackend(t *testing.T, srv *httptest.Server) embed.Backend {
	t.Helper()
	b, err := embed.Open("ollama", embed.Config{HTTPClient: srv.Client(), OllamaBaseURL: srv.URL})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	return b
}

func TestEmbedViaServerBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != embedPath {
			t.Errorf("path=%s, want %s", r.URL.Path, embedPath)
		}
		_, _ = w.Write([]byte(`{"model":"nomic-embed-text","embeddings":[[1,2],[3,4]],"prompt_eval_count":5}`))
	}))
	defer srv.Close()

	resp, err := newTestBackend(t, srv).Embed(context.Background(), embed.Request{Texts: []string{"a", "b"}}, nil)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(resp.Vectors) != 2 || resp.Dimensions != 2 || resp.Tokens != 5 {
		t.Fatalf("got vectors=%d dims=%d tokens=%d, want 2/2/5", len(resp.Vectors), resp.Dimensions, resp.Tokens)
	}
}

func TestEmbed4xxIsProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	_, err := newTestBackend(t, srv).Embed(context.Background(), embed.Request{Texts: []string{"x"}}, nil)
	if err == nil {
		t.Fatal("want error on 400")
	}
	if he, ok := err.(*embed.ProviderHTTPError); !ok || he.StatusCode != 400 {
		t.Fatalf("err=%T %v, want *ProviderHTTPError{400}", err, err)
	}
}

func TestEmbed5xxRetriesOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","embeddings":[[1,2,3]],"prompt_eval_count":1}`))
	}))
	defer srv.Close()

	resp, err := newTestBackend(t, srv).Embed(context.Background(), embed.Request{Texts: []string{"x"}}, nil)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if resp.Retries != 1 {
		t.Fatalf("retries=%d, want 1", resp.Retries)
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

// TestEmbedLive exercises a real local Ollama with the model the operator
// installed. Skipped when Ollama isn't reachable (CI, no-ollama dev machines).
func TestEmbedLive(t *testing.T) {
	b, err := embed.Open("ollama", embed.Config{HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	resp, err := b.Embed(context.Background(), embed.Request{
		Texts: []string{"search_query: cozy adventure", "search_document: a wolf-dog in the Yukon"},
	}, nil)
	if err != nil {
		if _, neterr := err.(*embed.ProviderNetError); neterr {
			t.Skipf("ollama not reachable at %s: %v", defaultBaseURL, err)
		}
		t.Fatalf("live embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("vectors=%d, want 2", len(resp.Vectors))
	}
	if resp.Dimensions != 768 {
		t.Fatalf("dimensions=%d, want 768 (nomic-embed-text)", resp.Dimensions)
	}
	t.Logf("live: model=%s dims=%d tokens=%d latency=%dms", resp.Model, resp.Dimensions, resp.Tokens, resp.LatencyMS)
}
