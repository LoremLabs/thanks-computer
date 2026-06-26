// Package openai is the hosted ai://embed backend: OpenAI's /v1/embeddings
// endpoint. It's the production counterpart to the local ollama backend —
// reachable from any node and authenticated, so prod nodes (which have no
// local Ollama) embed via `WITH provider = "openai"`.
//
// The cleartext API key (OPENAI_KEY) is read from the secrets.SecretBag once
// per call, placed in the Authorization header, and never stored on the
// backend, logged, or traced. A 401 body is discarded (providers occasionally
// echo a key prefix in error messages).
//
// `WITH dimensions = N` is forwarded to OpenAI's `dimensions` parameter, which
// the text-embedding-3 models honour (e.g. text-embedding-3-large → 1536).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/embed"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const (
	defaultModel    = "text-embedding-3-small"
	defaultEndpoint = "https://api.openai.com/v1/embeddings"
	secretName      = "OPENAI_KEY"
)

func init() {
	embed.Register("openai", func(cfg embed.Config) (embed.Backend, error) {
		if cfg.HTTPClient == nil {
			return nil, errors.New("openai: nil HTTPClient (embed.Config must carry the chassis-owned client)")
		}
		return &backend{httpClient: cfg.HTTPClient, endpoint: defaultEndpoint}, nil
	})
}

type backend struct {
	httpClient *http.Client
	endpoint   string
}

func (b *backend) Name() string              { return "openai" }
func (b *backend) Capabilities() []string    { return []string{"public_execution"} }
func (b *backend) DefaultModel() string      { return defaultModel }
func (b *backend) RequiredSecrets() []string { return []string{secretName} }

// Embed sends all input texts to OpenAI in one /v1/embeddings call. Retries
// once on transient errors (HTTP 5xx + network/timeout); not on 4xx.
func (b *backend) Embed(ctx context.Context, req embed.Request, bag *secrets.SecretBag) (embed.Response, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	if len(req.Texts) == 0 {
		return embed.Response{Provider: b.Name(), Model: model}, &embed.InvalidWithError{Reason: "no input text to embed"}
	}
	if bag == nil {
		return embed.Response{Provider: b.Name(), Model: model}, &embed.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}
	apiKey, ok := bag.Get(secretName)
	if !ok || len(apiKey) == 0 {
		return embed.Response{Provider: b.Name(), Model: model}, &embed.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}

	outbound := map[string]any{"model": model, "input": req.Texts}
	if req.Dimensions > 0 {
		// text-embedding-3 models honour this (Matryoshka truncation).
		outbound["dimensions"] = req.Dimensions
	}
	bodyBytes, err := json.Marshal(outbound)
	if err != nil {
		return embed.Response{Provider: b.Name(), Model: model}, fmt.Errorf("openai: marshal request: %w", err)
	}

	start := time.Now()
	resp, retries, runErr := b.doWithSimpleRetry(ctx, apiKey, bodyBytes)
	latencyMS := time.Since(start).Milliseconds()
	if runErr != nil {
		return embed.Response{Provider: b.Name(), Model: model, LatencyMS: latencyMS, Retries: retries}, runErr
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return embed.Response{Provider: b.Name(), Model: model, LatencyMS: latencyMS, Retries: retries},
			&embed.ProviderHTTPError{StatusCode: resp.StatusCode, Body: sanitizeBody(respBody)}
	}
	return parseOpenAIEmbed(respBody, b.Name(), model, latencyMS, retries)
}

func (b *backend) doWithSimpleRetry(ctx context.Context, apiKey, body []byte) (*http.Response, int, error) {
	attempt := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		// Cleartext touches only this header.Set; never stored/logged. The
		// processor's bag.Zero() wipes the underlying bytes on Run exit.
		httpReq.Header.Set("Authorization", "Bearer "+string(apiKey))
		return b.httpClient.Do(httpReq)
	}

	resp, err := attempt()
	if err == nil && resp.StatusCode < 500 {
		return resp, 0, nil
	}
	if err == nil {
		_ = resp.Body.Close()
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

// parseOpenAIEmbed decodes OpenAI's /v1/embeddings response, ordering vectors
// by the `index` field (defensive — the API returns input order, but index is
// authoritative).
func parseOpenAIEmbed(body []byte, provider, model string, latencyMS int64, retries int) (embed.Response, error) {
	partial := embed.Response{Provider: provider, Model: model, LatencyMS: latencyMS, Retries: retries}
	if len(body) == 0 {
		return partial, &embed.ProviderParseError{Reason: "empty body", BodyLen: 0}
	}

	var raw struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return partial, &embed.ProviderParseError{Reason: err.Error(), BodyLen: len(body)}
	}
	if len(raw.Data) == 0 {
		return partial, &embed.ProviderParseError{Reason: "no embeddings in response", BodyLen: len(body)}
	}

	sort.Slice(raw.Data, func(i, j int) bool { return raw.Data[i].Index < raw.Data[j].Index })
	vectors := make([][]float32, len(raw.Data))
	for i, d := range raw.Data {
		vectors[i] = d.Embedding
	}

	out := embed.Response{
		Vectors:    vectors,
		Provider:   provider,
		Model:      model,
		Tokens:     raw.Usage.PromptTokens,
		LatencyMS:  latencyMS,
		Retries:    retries,
		Dimensions: len(vectors[0]),
	}
	if raw.Model != "" {
		out.Model = raw.Model
	}
	return out, nil
}

// sanitizeBody scrubs provider error bodies that may quote a key prefix, and
// bounds the size. Mirrors the openrouter backend's defense in depth.
func sanitizeBody(body []byte) string {
	low := strings.ToLower(string(body))
	if strings.Contains(low, "api key") || strings.Contains(low, "api_key") ||
		strings.Contains(low, "bearer ") || strings.Contains(low, "incorrect api key") ||
		strings.Contains(low, "invalid_api_key") || strings.Contains(low, "unauthorized") {
		return `{"error":"authentication or key-related provider error (body sanitized)"}`
	}
	if len(body) > 2048 {
		return string(body[:2048]) + "...(truncated)"
	}
	return string(body)
}
