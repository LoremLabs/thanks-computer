// Package ollama is the v1 ai://embed backend: a local, keyless Ollama
// instance (https://ollama.com) speaking its /api/embed endpoint.
//
// It is the developer/offline default — `nomic-embed-text` (768-dim) over
// http://localhost:11434 needs no API key, so embedding works out of the box
// in `txco dev` and for offline catalog work. The OpenAI-direct backend
// (which needs OPENAI_KEY) is a follow-up for deployments where a local
// Ollama isn't reachable.
//
// The backend self-registers in init() under the name "ollama"; the chassis
// activates it with a blank import.
//
// **Task prefixes.** nomic-embed-text expects `search_document:` /
// `search_query:` prefixes for best retrieval quality. That is a per-call,
// author-visible decision: the txcl author prepends the prefix in the WITH
// value (e.g. `WITH text = &concat("search_query: ", @web.req.body)`). The
// backend stays dumb and embeds exactly what it is given.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/embed"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// defaultModel is used when WITH model = "..." is absent.
const defaultModel = "nomic-embed-text"

// defaultBaseURL is the local Ollama address. Overridden by
// embed.Config.OllamaBaseURL (chassis config embed-ollama-base-url).
const defaultBaseURL = "http://localhost:11434"

// embedPath is Ollama's batch embedding endpoint (input may be a string or an
// array of strings; we always send an array).
const embedPath = "/api/embed"

func init() {
	embed.Register("ollama", func(cfg embed.Config) (embed.Backend, error) {
		if cfg.HTTPClient == nil {
			return nil, errors.New("ollama: nil HTTPClient (embed.Config must carry the chassis-owned client)")
		}
		base := cfg.OllamaBaseURL
		if base == "" {
			base = defaultBaseURL
		}
		return &backend{httpClient: cfg.HTTPClient, baseURL: strings.TrimRight(base, "/")}, nil
	})
}

type backend struct {
	httpClient *http.Client
	baseURL    string
}

func (b *backend) Name() string              { return "ollama" }
func (b *backend) Capabilities() []string    { return []string{"local_execution"} }
func (b *backend) DefaultModel() string      { return defaultModel }
func (b *backend) RequiredSecrets() []string { return nil } // local, unauthenticated

// Embed sends all input texts to Ollama in one /api/embed call. Retries once
// on transient errors (HTTP 5xx + network/timeout); does not retry on 4xx.
// The secret bag is unused (local endpoint) but accepted to satisfy the
// interface.
func (b *backend) Embed(ctx context.Context, req embed.Request, _ *secrets.SecretBag) (embed.Response, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	if len(req.Texts) == 0 {
		return embed.Response{Provider: b.Name(), Model: model}, &embed.InvalidWithError{Reason: "no input text to embed"}
	}

	outbound := map[string]any{
		"model": model,
		"input": req.Texts,
	}
	bodyBytes, err := json.Marshal(outbound)
	if err != nil {
		return embed.Response{Provider: b.Name(), Model: model}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	start := time.Now()
	resp, retries, runErr := b.doWithSimpleRetry(ctx, bodyBytes)
	latencyMS := time.Since(start).Milliseconds()
	if runErr != nil {
		return embed.Response{Provider: b.Name(), Model: model, LatencyMS: latencyMS, Retries: retries}, runErr
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return embed.Response{Provider: b.Name(), Model: model, LatencyMS: latencyMS, Retries: retries},
			&embed.ProviderHTTPError{StatusCode: resp.StatusCode, Body: truncate(respBody)}
	}

	return parseOllamaEmbed(respBody, b.Name(), model, latencyMS, retries)
}

// doWithSimpleRetry runs the request once, retrying once on HTTP 5xx or
// network error. Mirrors the openrouter backend's policy.
func (b *backend) doWithSimpleRetry(ctx context.Context, body []byte) (*http.Response, int, error) {
	url := b.baseURL + embedPath
	attempt := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		return b.httpClient.Do(httpReq)
	}

	resp, err := attempt()
	if err == nil && resp.StatusCode < 500 {
		return resp, 0, nil
	}
	if err == nil {
		_ = resp.Body.Close() // 5xx — close before retry
	}
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		return nil, 0, &embed.ProviderNetError{Reason: ctx.Err().Error()}
	}

	resp2, err2 := attempt()
	if err2 != nil {
		return nil, 1, &embed.ProviderNetError{Reason: err2.Error()}
	}
	return resp2, 1, nil
}

// parseOllamaEmbed decodes Ollama's /api/embed response:
//
//	{"model":"nomic-embed-text","embeddings":[[...]],"prompt_eval_count":N}
//
// The chassis records the actual dimension (len of the first vector) so a
// model swap is observable without trusting the request.
func parseOllamaEmbed(body []byte, provider, model string, latencyMS int64, retries int) (embed.Response, error) {
	partial := embed.Response{Provider: provider, Model: model, LatencyMS: latencyMS, Retries: retries}
	if len(body) == 0 {
		return partial, &embed.ProviderParseError{Reason: "empty body", BodyLen: 0}
	}

	var raw struct {
		Model      string      `json:"model"`
		Embeddings [][]float32 `json:"embeddings"`
		Tokens     int64       `json:"prompt_eval_count"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return partial, &embed.ProviderParseError{Reason: err.Error(), BodyLen: len(body)}
	}
	if len(raw.Embeddings) == 0 {
		return partial, &embed.ProviderParseError{Reason: "no embeddings in response", BodyLen: len(body)}
	}

	out := embed.Response{
		Vectors:   raw.Embeddings,
		Provider:  provider,
		Model:     model,
		Tokens:    raw.Tokens,
		LatencyMS: latencyMS,
		Retries:   retries,
	}
	if raw.Model != "" {
		out.Model = raw.Model
	}
	out.Dimensions = len(raw.Embeddings[0])
	return out, nil
}

// truncate bounds a provider error body so envelopes stay small. Ollama error
// bodies don't quote secrets (the endpoint is keyless), so no scrubbing is
// needed beyond a length cap.
func truncate(body []byte) string {
	const max = 2048
	if len(body) > max {
		return string(body[:max]) + "...(truncated)"
	}
	return string(body)
}
