// Package openai is a native OpenAI-direct ai://chat backend.
//
// It speaks the OpenAI chat-completions API directly
// (https://api.openai.com/v1/chat/completions) using the same OPENAI_KEY
// secret the ai://embed openai backend uses — so a tenant that embeds with
// OpenAI can also chat with OpenAI under one key/account, instead of routing
// chat through a second provider. Mirror of chassis/chat/openrouter (the API is
// the same OpenAI-compatible shape); the only differences are the endpoint, the
// secret name, the default model, the absence of OpenRouter's accounting
// headers, and JSON mode for a schema call (see jsonMode).
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/chat/openaicompat"
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

// ToolFeatures: OpenAI takes tools, lets a request bound parallel calls,
// and decodes strictly when asked.
func (b *backend) ToolFeatures() chat.ToolFeatures {
	return chat.ToolFeatures{Tools: true, Parallel: true, Strict: true}
}

// options are the dialect fields OpenAI's endpoint takes beyond the shared
// shape: JSON mode for a qualifying schema call, the prompt cache key, and
// strict decoding of tool arguments and of a schema answer when asked.
var options = openaicompat.Options{JSONMode: true, PromptCacheKey: true, ToolStrictField: true, JSONSchemaFormat: true}

// Run executes one chat completion against OpenAI. Retries once on transient
// errors (HTTP 5xx + network/timeout); does not retry on 4xx. The schema itself
// is NOT sent — ExecAI post-validates Response.Text against req.Schema, so the
// backend stays dumb and provider-swappable (mirrors openrouter). A schema call
// asks only for JSON mode (jsonMode), which makes the text well-formed JSON.
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

	// The body is the OpenAI-compatible dialect's (chat/openaicompat).
	bodyBytes, err := openaicompat.BuildBody(req, model, options)
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

	out, perr := parseOpenAICompatResponse(respBody, b.Name(), model, latencyMS, retries)
	if perr != nil {
		return out, perr
	}
	// Undo the nullable wrapping strict decoding needed, and say how the
	// schema went out.
	openaicompat.ApplyStrict(req, options, &out)
	out.SchemaMode = openaicompat.SchemaMode(req, options)
	return out, nil
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

// parseOpenAICompatResponse and sanitizeBody are the dialect's
// (chat/openaicompat), kept under their old names so the backend reads as
// before and its tests call them as before.
func parseOpenAICompatResponse(body []byte, provider, model string, latencyMS int64, retries int) (chat.Response, error) {
	return openaicompat.ParseResponse(body, provider, model, latencyMS, retries)
}

func sanitizeBody(body []byte) string { return openaicompat.SanitizeBody(body) }
