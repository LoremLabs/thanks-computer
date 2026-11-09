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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/chat/openaicompat"
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

func (b *backend) Name() string              { return "openrouter" }
func (b *backend) Capabilities() []string    { return []string{"public_execution"} }
func (b *backend) RequiredSecrets() []string { return []string{secretName} }

// ToolFeatures: OpenRouter passes tools and parallel_tool_calls through to
// its upstreams; strict decoding is not promised, since a gateway that
// normalizes tool schemas can drop or mangle the field.
func (b *backend) ToolFeatures() chat.ToolFeatures {
	return chat.ToolFeatures{Tools: true, Parallel: true, Strict: false}
}

// options: nothing beyond the shared shape except, when tools are offered,
// the routing field that keeps the request on an upstream that takes them.
var options = openaicompat.Options{RequireParameters: true}

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

	// The body is the OpenAI-compatible dialect's (chat/openaicompat) with
	// no optional field on: the schema is not sent as `response_format`
	// (ExecAI post-validates Response.Text against req.Schema), and
	// req.CacheKey is not sent, since OpenRouter's chat-completions API does
	// not document `prompt_cache_key` (its OpenAI caching is automatic) and
	// an unknown field could be refused or silently dropped upstream.
	bodyBytes, err := openaicompat.BuildBody(req, model, options)
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

	out, perr := parseOpenAICompatResponse(respBody, b.Name(), model, latencyMS, retries)
	if perr != nil {
		return out, perr
	}
	// No strict decoding through OpenRouter (options leave both strict
	// fields off), so this changes nothing today; it keeps the two
	// backends the same shape.
	openaicompat.ApplyStrict(req, options, &out)
	out.SchemaMode = openaicompat.SchemaMode(req, options)
	return out, nil
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

// parseOpenAICompatResponse is the dialect's (chat/openaicompat), kept
// under its old name. An empty 2xx body (seen on the free tier's rate
// limit, which answers 200 with no content instead of a 429) and malformed
// JSON are coded ProviderParseErrors (txco_chat_provider_parse), so authors
// can tell an upstream flake from a real reply and fall back to a paid
// model; absent `choices` or `usage` give a partial Response with an empty
// Text that a WHEN can handle.
func parseOpenAICompatResponse(body []byte, provider, model string, latencyMS int64, retries int) (chat.Response, error) {
	return openaicompat.ParseResponse(body, provider, model, latencyMS, retries)
}

// sanitizeBody is the dialect's (chat/openaicompat), kept under its old
// name: it scrubs an error body that may quote a prefix of the key and
// bounds its size. The chassis's defense in depth: the SecretBag
// panic-on-marshal makes JSON exfiltration impossible; this reduces the
// chance of a non-JSON path reaching a key prefix.
func sanitizeBody(body []byte) string { return openaicompat.SanitizeBody(body) }
