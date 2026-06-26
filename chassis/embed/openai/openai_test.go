package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/embed"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

func bagWith(key string) *secrets.SecretBag {
	var b secrets.SecretBag
	b.Set(secretName, []byte(key))
	return &b
}

func TestParseOpenAIEmbedOrdersByIndex(t *testing.T) {
	body := []byte(`{"data":[{"index":1,"embedding":[0.3,0.4]},{"index":0,"embedding":[0.1,0.2]}],"model":"text-embedding-3-small","usage":{"prompt_tokens":9}}`)
	resp, err := parseOpenAIEmbed(body, "openai", "text-embedding-3-small", 5, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Vectors) != 2 || resp.Dimensions != 2 || resp.Tokens != 9 {
		t.Fatalf("got %+v", resp)
	}
	if resp.Vectors[0][0] != 0.1 { // index 0 must sort first
		t.Fatalf("index ordering wrong: %v", resp.Vectors)
	}
}

func TestEmbedForwardsDimensionsAndAuth(t *testing.T) {
	var gotAuth string
	var gotDims float64
	var gotInputLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		bs, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(bs, &req)
		if d, ok := req["dimensions"].(float64); ok {
			gotDims = d
		}
		if in, ok := req["input"].([]any); ok {
			gotInputLen = len(in)
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2,3]},{"index":1,"embedding":[4,5,6]}],"model":"text-embedding-3-large","usage":{"prompt_tokens":12}}`))
	}))
	defer srv.Close()

	b := &backend{httpClient: srv.Client(), endpoint: srv.URL}
	resp, err := b.Embed(context.Background(), embed.Request{
		Texts: []string{"a", "b"}, Model: "text-embedding-3-large", Dimensions: 1536,
	}, bagWith("sk-test"))
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(resp.Vectors) != 2 || resp.Dimensions != 3 {
		t.Fatalf("got vectors=%d dims=%d", len(resp.Vectors), resp.Dimensions)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth header=%q", gotAuth)
	}
	if gotDims != 1536 {
		t.Fatalf("dimensions not forwarded: %v", gotDims)
	}
	if gotInputLen != 2 {
		t.Fatalf("batch input len=%d, want 2", gotInputLen)
	}
}

func TestEmbedMissingSecret(t *testing.T) {
	b := &backend{httpClient: http.DefaultClient, endpoint: defaultEndpoint}
	if _, err := b.Embed(context.Background(), embed.Request{Texts: []string{"x"}}, nil); err == nil {
		t.Fatal("nil bag: want MissingSecretError")
	} else if _, ok := err.(*embed.MissingSecretError); !ok {
		t.Fatalf("err=%T, want *MissingSecretError", err)
	}
}

func TestEmbed401BodySanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-abc123..."}}`))
	}))
	defer srv.Close()

	b := &backend{httpClient: srv.Client(), endpoint: srv.URL}
	_, err := b.Embed(context.Background(), embed.Request{Texts: []string{"x"}}, bagWith("sk-test"))
	he, ok := err.(*embed.ProviderHTTPError)
	if !ok || he.StatusCode != 401 {
		t.Fatalf("err=%T %v, want *ProviderHTTPError{401}", err, err)
	}
	if strings.Contains(he.Body, "sk-abc") {
		t.Fatalf("key prefix leaked in sanitized body: %s", he.Body)
	}
}
