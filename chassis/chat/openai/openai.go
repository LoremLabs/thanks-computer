// Package openai is a native OpenAI-direct ai://chat backend.
//
// It speaks the OpenAI chat-completions API directly
// (https://api.openai.com/v1/chat/completions) using the same OPENAI_KEY
// secret the ai://embed openai backend uses — so a tenant that embeds with
// OpenAI can also chat with OpenAI under one key/account, instead of routing
// chat through a second provider. Mirror of chassis/chat/openrouter (the API is
// the same OpenAI-compatible shape); the only differences are the endpoint, the
// secret name, the default model, and the absence of OpenRouter's accounting
// headers.
//
// The backend self-registers in init() under the name "openai"; the chassis
// activates it with a blank import in the main package.
//
// **Leak posture** is identical to the openrouter backend: the cleartext key is
// read from the SecretBag once per Run, set only on the outbound Authorization
// header, never stored / logged / traced, and any auth-error body is discarded.
package openai

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

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// defaultModel is used when WITH model = "..." is absent. A small, fast model is
// the right default for a "boring" chat call.
const defaultModel = "gpt-4o-mini"

// endpoint is the OpenAI chat-completions URL. Hard-coded — egress-layer URL
// allowlisting is its own follow-up; for v1 the implicit allowlist is this constant.
const endpoint = "https://api.openai.com/v1/chat/completions"

// secretName is the standardized per-backend secret name — the SAME one the
// ai://embed openai backend reads, so a tenant configures OpenAI once.
const secretName = "OPENAI_KEY"

func init() {
	chat.Register("openai", func(cfg chat.Config) (chat.Backend, error) {
		if cfg.HTTPClient == nil {
			return nil, errors.New("openai: nil HTTPClient (chat.Config must carry the chassis-owned client)")
		}
		return &backend{httpClient: cfg.HTTPClient}, nil
	})
}

type backend struct {
	httpClient *http.Client
}

func (b *backend) Name() string              { return "openai" }
func (b *backend) Capabilities() []string    { return []string{"public_execution"} }
func (b *backend) RequiredSecrets() []string { return []string{secretName} }

// Run executes one chat completion against OpenAI. Retries once on transient
// errors (HTTP 5xx + network/timeout); does not retry on 4xx. Schema is NOT sent
// as response_format — ExecAI post-validates Response.Text against req.Schema, so
// the backend stays dumb and provider-swappable (mirrors openrouter).
func (b *backend) Run(ctx context.Context, req chat.Request, bag *secrets.SecretBag) (chat.Response, error) {
	if bag == nil {
		return chat.Response{}, errors.New("openai: nil secret bag")
	}
	apiKey, ok := bag.Get(secretName)
	if !ok || len(apiKey) == 0 {
		return chat.Response{}, &chat.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}

	model := req.Model
	if model == "" {
		model = defaultModel
	}

	outbound := map[string]any{
		"model":    model,
		"messages": req.Messages,
	}
	bodyBytes, err := json.Marshal(outbound)
	if err != nil {
		return chat.Response{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	start := time.Now()
	resp, retries, runErr := b.doWithSimpleRetry(ctx, apiKey, bodyBytes)
	latencyMS := time.Since(start).Milliseconds()
	if runErr != nil {
		return chat.Response{Retries: retries, LatencyMS: latencyMS, Provider: b.Name(), Model: model}, runErr
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusUnauthorized {
		// Discard body — may quote a prefix of the submitted key.
		return chat.Response{Retries: retries, LatencyMS: latencyMS, Provider: b.Name(), Model: model}, chat.ErrAuthFailed
	}
	if resp.StatusCode >= 400 {
		return chat.Response{Retries: retries, LatencyMS: latencyMS, Provider: b.Name(), Model: model},
			&chat.ProviderHTTPError{StatusCode: resp.StatusCode, Body: sanitizeBody(respBody)}
	}

	return parseOpenAICompatResponse(respBody, b.Name(), model, latencyMS, retries)
}

// doWithSimpleRetry runs the request once and retries once on HTTP 5xx or
// network/DNS/timeout error. 4xx responses do NOT trigger a retry. Backoff is
// 250ms, ctx-cancelable.
func (b *backend) doWithSimpleRetry(ctx context.Context, apiKey []byte, body []byte) (*http.Response, int, error) {
	attempt := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		// Cleartext touches only this single header.Set call; never stored,
		// never logged. The bag.Zero() in the processor wipes the bytes on exit.
		httpReq.Header.Set("Authorization", "Bearer "+string(apiKey))
		return b.httpClient.Do(httpReq)
	}

	resp, err := attempt()
	if err == nil && resp.StatusCode < 500 {
		return resp, 0, nil
	}

	if err == nil {
		_ = resp.Body.Close() // 5xx — close before retrying
	}
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		return nil, 0, &chat.ProviderNetError{Reason: ctx.Err().Error()}
	}

	resp2, err2 := attempt()
	if err2 != nil {
		return nil, 1, &chat.ProviderNetError{Reason: err2.Error()}
	}
	return resp2, 1, nil
}

// parseOpenAICompatResponse decodes an OpenAI-shaped chat completion body into a
// chat.Response. Empty/malformed body surfaces as a coded ProviderParseError.
// Resilient to absent choices/usage. (Same logic as the openrouter backend —
// each backend is self-contained, like the embed backends.)
func parseOpenAICompatResponse(body []byte, provider, model string, latencyMS int64, retries int) (chat.Response, error) {
	partial := chat.Response{Provider: provider, Model: model, LatencyMS: latencyMS, Retries: retries}
	if len(body) == 0 {
		return partial, &chat.ProviderParseError{Reason: "empty body", BodyLen: 0}
	}

	var raw struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return partial, &chat.ProviderParseError{Reason: err.Error(), BodyLen: len(body)}
	}

	out := chat.Response{
		Provider:  provider,
		Model:     raw.Model, // prefer the provider's echo (the resolved model)
		TokensIn:  raw.Usage.PromptTokens,
		TokensOut: raw.Usage.CompletionTokens,
		LatencyMS: latencyMS,
		Retries:   retries,
	}
	if out.Model == "" {
		out.Model = model
	}
	if len(raw.Choices) > 0 {
		out.Text = raw.Choices[0].Message.Content
	}
	return out, nil
}

// sanitizeBody scrubs error responses that may quote a prefix of the submitted
// API key, and bounds the size. (Mirrors the openrouter backend.)
func sanitizeBody(body []byte) string {
	low := strings.ToLower(string(body))
	if strings.Contains(low, "api_key") ||
		strings.Contains(low, "api key") ||
		strings.Contains(low, "bearer ") ||
		strings.Contains(low, "invalid_api_key") ||
		strings.Contains(low, "incorrect api key") ||
		strings.Contains(low, "unauthorized") {
		return `{"error":"authentication or key-related provider error (body sanitized)"}`
	}
	if len(body) > 2048 {
		return string(body[:2048]) + `...(truncated)`
	}
	return string(body)
}
