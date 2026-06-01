// Package openrouter is the v1 ai://chat backend.
//
// It speaks the OpenAI-compatible chat-completions API to OpenRouter's
// public endpoint (https://openrouter.ai/api/v1/chat/completions),
// which proxies to dozens of upstream model providers. One backend,
// many models — a deliberately conservative v1 choice (the spec's
// "boring foundation").
//
// The backend self-registers in init() under the name "openrouter".
// The chassis activates it with a blank import in the main package.
//
// **Leak posture.** The cleartext API key (OPENROUTER_KEY) is read from
// the secrets.SecretBag exactly once per Run, passed into the
// outbound http.Request's Authorization header, and never stored on
// the backend struct, never written to a trace field, never logged.
// A 401 response body is discarded immediately — providers occasionally
// quote a prefix of the submitted key in error messages, and that
// prefix would otherwise reach the response envelope and trace.
package openrouter

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

// defaultModel is used when WITH model = "..." is absent. A small, fast
// model is the right default for a "boring" chat call.
const defaultModel = "openai/gpt-4o-mini"

// endpoint is the OpenRouter chat-completions URL. Hard-coded — URL
// allowlisting at the egress layer is its own follow-up PR; for v1 the
// implicit allowlist is "this constant."
const endpoint = "https://openrouter.ai/api/v1/chat/completions"

// secretName is the standardized per-backend secret name. Matches the
// spec's table at thanks-computer-service/docs/todo-chat-exec.md.
const secretName = "OPENROUTER_KEY"

func init() {
	chat.Register("openrouter", func(cfg chat.Config) (chat.Backend, error) {
		if cfg.HTTPClient == nil {
			return nil, errors.New("openrouter: nil HTTPClient (chat.Config must carry the chassis-owned client)")
		}
		return &backend{httpClient: cfg.HTTPClient}, nil
	})
}

type backend struct {
	httpClient *http.Client
}

func (b *backend) Name() string             { return "openrouter" }
func (b *backend) Capabilities() []string   { return []string{"public_execution"} }
func (b *backend) RequiredSecrets() []string { return []string{secretName} }

// Run executes one chat completion against OpenRouter. Retries once on
// transient errors (HTTP 5xx + network/timeout); does not retry on 4xx.
// Sophisticated retry semantics (rate-limit-aware backoff, context-
// overflow distinction, refusal handling) are deferred to a follow-up.
func (b *backend) Run(ctx context.Context, req chat.Request, bag *secrets.SecretBag) (chat.Response, error) {
	if bag == nil {
		return chat.Response{}, errors.New("openrouter: nil secret bag")
	}
	apiKey, ok := bag.Get(secretName)
	if !ok || len(apiKey) == 0 {
		// In practice the ExecAI handler materializes RequiredSecrets()
		// before this Run, so this is defensive — an empty bag here is
		// a chassis wiring bug, not a user-facing condition.
		return chat.Response{}, &chat.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}

	model := req.Model
	if model == "" {
		model = defaultModel
	}

	// Build OpenAI-compatible body. Schema is intentionally NOT sent as
	// `response_format` in v1 — ExecAI post-validates Response.Text
	// against req.Schema. Keeping the backend dumb here makes provider
	// swap-out trivial (the same body is what every OpenAI-compatible
	// endpoint accepts) and pushes schema concerns into one place.
	outbound := map[string]any{
		"model":    model,
		"messages": req.Messages,
	}
	bodyBytes, err := json.Marshal(outbound)
	if err != nil {
		return chat.Response{}, fmt.Errorf("openrouter: marshal request: %w", err)
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
		// Returning the sanitized sentinel keeps cleartext out of the
		// response envelope and the trace event.
		return chat.Response{Retries: retries, LatencyMS: latencyMS, Provider: b.Name(), Model: model}, chat.ErrAuthFailed
	}
	if resp.StatusCode >= 400 {
		return chat.Response{Retries: retries, LatencyMS: latencyMS, Provider: b.Name(), Model: model},
			&chat.ProviderHTTPError{StatusCode: resp.StatusCode, Body: sanitizeBody(respBody)}
	}

	return parseOpenAICompatResponse(respBody, b.Name(), model, latencyMS, retries)
}

// doWithSimpleRetry runs the request once and retries once on HTTP 5xx
// or network/DNS/timeout error. 4xx responses do NOT trigger a retry
// (the error is the caller's fault, not the network's). Backoff is
// 250ms, ctx-cancelable.
func (b *backend) doWithSimpleRetry(ctx context.Context, apiKey []byte, body []byte) (*http.Response, int, error) {
	attempt := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		// Cleartext touches only this single header.Set call; it is
		// never stored on the backend struct, never on context, never
		// logged. The bag.Zero() in the processor wipes the underlying
		// bytes on Run exit.
		httpReq.Header.Set("Authorization", "Bearer "+string(apiKey))
		// OpenRouter convention — they ask for these for accounting on
		// the free tier; harmless either way.
		httpReq.Header.Set("HTTP-Referer", "https://thanks.computer")
		httpReq.Header.Set("X-Title", "txco-chassis")
		return b.httpClient.Do(httpReq)
	}

	resp, err := attempt()
	if err == nil && resp.StatusCode < 500 {
		return resp, 0, nil
	}

	// Decide whether to retry. Network error → yes. 5xx → yes. 4xx → no
	// (handled by the early return above).
	if err != nil {
		// Network/DNS/timeout. Sleep then retry.
	} else {
		// 5xx. Close the body before retrying.
		_ = resp.Body.Close()
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

// parseOpenAICompatResponse decodes an OpenAI-shaped chat completion
// body into a chat.Response. Empty body or malformed JSON surfaces as
// a coded ProviderParseError (txco_chat_provider_parse) so rule authors
// can distinguish upstream-flake from a real reply. v1 does NOT retry
// on parse errors — that's a sophistication for the broader retry-
// policy PR. Resilient to absent `choices` / `usage` within otherwise-
// valid JSON: returns a partial Response with an empty Text so rule
// authors can WHEN-handle the empty case.
func parseOpenAICompatResponse(body []byte, provider, model string, latencyMS int64, retries int) (chat.Response, error) {
	partial := chat.Response{Provider: provider, Model: model, LatencyMS: latencyMS, Retries: retries}

	// Empty 2xx body — seen on :free-tier rate-limit; upstream returns
	// 200 with no content instead of a 429. Surface clearly so authors
	// can fall back to a paid model.
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
		Model:     raw.Model, // prefer the provider's echo (may be a more specific routed model)
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

// sanitizeBody scrubs common patterns that may quote a prefix of the
// submitted API key in error responses. Matches Anthropic / OpenAI /
// OpenRouter shapes; falls back to discarding any body whose lowered
// form contains "api_key" / "api key" / "bearer ".
//
// The chassis's defense in depth: the SecretBag panic-on-marshal makes
// JSON exfiltration impossible; this function reduces the chance of a
// non-JSON path (logged via zap.String somewhere downstream) reaching
// a key prefix.
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
	// Bound the size to keep envelopes small even when the provider
	// returns a verbose error.
	if len(body) > 2048 {
		return string(body[:2048]) + `...(truncated)`
	}
	return string(body)
}
