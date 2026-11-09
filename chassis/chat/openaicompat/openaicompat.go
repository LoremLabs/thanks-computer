// Package openaicompat is the OpenAI Chat Completions dialect of the
// chassis's chat request: how a chat.Request becomes the JSON body every
// OpenAI-compatible endpoint accepts, and how such an endpoint's answer
// becomes a chat.Response. The openai and openrouter backends both speak it;
// each keeps only what differs between them (endpoint, secret, headers,
// default model, retry) and calls here for the body and the parse.
//
// Pure: no HTTP, no secrets. A backend hands in a Request and gets bytes.
package openaicompat

import (
	"encoding/json"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/chat"
)

// Options says which optional fields a backend's endpoint takes. A field
// that is off is never written, so the body a backend sent before this
// package existed is the body it sends now.
type Options struct {
	// JSONMode asks for `response_format: {type: json_object}` on a schema
	// call that qualifies (see JSONMode). OpenAI takes it; OpenRouter is
	// left as it was, without it.
	JSONMode bool
	// PromptCacheKey sends req.CacheKey as `prompt_cache_key`. OpenAI routes
	// same-key requests to the same cache; OpenRouter does not document the
	// field, so it stays off there.
	PromptCacheKey bool
	// RequireParameters adds OpenRouter's routing field
	// `provider: {require_parameters: true}` when tools are offered, so the
	// request goes only to an upstream that takes them, or fails, instead
	// of being routed to one that drops them.
	RequireParameters bool
	// ToolStrictField lets a definition carry `strict` (OpenAI's constrained
	// decoding). Off, the field is never written; gateways that normalize
	// tool schemas can mangle it.
	ToolStrictField bool
	// JSONSchemaFormat lets a schema call under strict decoding go out as
	// `response_format: {type: json_schema, strict: true}`. Off, a schema
	// call is JSON mode at most, as before.
	JSONSchemaFormat bool
}

// BuildBody renders the request body for model. The outer object is a map,
// so its keys marshal in alphabetical order: that is the order the backends
// always produced, and the byte-identity test pins it. Without tools the
// body is what it was before tools existed; the tool fields are written only
// when req.Tools is non-empty.
func BuildBody(req chat.Request, model string, o Options) ([]byte, error) {
	outbound := map[string]any{
		"model":    model,
		"messages": wireMessages(req.Messages),
	}
	// A schema call: strictly decoded against the schema when the request
	// asks for strict decoding and the schema qualifies; else JSON mode, as
	// before, when the call qualifies for that.
	if plan, ok := strictSchema(req, o); ok {
		outbound["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "answer",
				"strict": true,
				"schema": plan.schema,
			},
		}
	} else if o.JSONMode && JSONMode(req) {
		outbound["response_format"] = map[string]string{"type": "json_object"}
	}
	if o.PromptCacheKey && req.CacheKey != "" {
		outbound["prompt_cache_key"] = req.CacheKey
	}
	if len(req.Tools) > 0 {
		outbound["tools"] = wireTools(req.Tools, strictTools(req, o))
		if req.ToolChoice != "" {
			outbound["tool_choice"] = wireToolChoice(req.ToolChoice)
		}
		if req.Parallel != nil {
			outbound["parallel_tool_calls"] = *req.Parallel
		}
		if o.RequireParameters {
			outbound["provider"] = map[string]any{"require_parameters": true}
		}
	}
	return json.Marshal(outbound)
}

// JSONMode reports whether a call asks for JSON mode, under which the reply
// is always well-formed JSON. Without it a model can break the syntax (on
// 2026-10-20 a schema call answered invalid JSON twice running), and the
// schema check can only refuse the answer. Only for a schema whose top level
// is an object, since JSON mode answers nothing else, and only when a
// message the author or the model wrote mentions JSON: OpenAI refuses JSON
// mode (400) otherwise. A tool result is not read for the word: a
// capability's answer that happens to contain "json" must not turn the
// mode on.
func JSONMode(req chat.Request) bool {
	if len(req.Schema) == 0 {
		return false
	}
	var top struct {
		Type any `json:"type"`
	}
	if json.Unmarshal(req.Schema, &top) != nil || top.Type != "object" {
		return false
	}
	for _, m := range req.Messages {
		if strings.EqualFold(m.Role, "tool") {
			continue
		}
		if strings.Contains(strings.ToLower(m.Content), "json") {
			return true
		}
	}
	return false
}

// SchemaMode says how BuildBody sent a schema call with these options:
// "json_schema" (strict), "json_object" (JSON mode), or "" (no schema, or
// neither applied). Backends stamp it on the response.
func SchemaMode(req chat.Request, o Options) string {
	if len(req.Schema) == 0 {
		return ""
	}
	if _, ok := strictSchema(req, o); ok {
		return "json_schema"
	}
	if o.JSONMode && JSONMode(req) {
		return "json_object"
	}
	return ""
}

// ParseResponse decodes an OpenAI-shaped chat completion body into a
// chat.Response. An empty or malformed body is a coded ProviderParseError;
// absent choices or usage are tolerated. provider and model name the call
// for the partial response an error carries; the provider's own `model`
// echo wins when present (it is the resolved model).
func ParseResponse(body []byte, provider, model string, latencyMS int64, retries int) (chat.Response, error) {
	partial := chat.Response{Provider: provider, Model: model, LatencyMS: latencyMS, Retries: retries}
	if len(body) == 0 {
		return partial, &chat.ProviderParseError{Reason: "empty body", BodyLen: 0}
	}

	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return partial, &chat.ProviderParseError{Reason: err.Error(), BodyLen: len(body)}
	}

	out := chat.Response{
		Provider:        provider,
		Model:           raw.Model,
		ResponseID:      raw.ID,
		TokensIn:        raw.Usage.PromptTokens,
		TokensOut:       raw.Usage.CompletionTokens,
		TokensCached:    raw.Usage.PromptTokensDetails.CachedTokens,
		TokensReasoning: raw.Usage.CompletionTokensDetails.ReasoningTokens,
		LatencyMS:       latencyMS,
		Retries:         retries,
	}
	if out.Model == "" {
		out.Model = model
	}
	if len(raw.Choices) > 0 {
		c := raw.Choices[0]
		out.Text = c.Message.Content
		out.Finish = mapFinish(c.FinishReason)
		for _, tc := range c.Message.ToolCalls {
			call := chat.ToolCall{ID: tc.ID, Name: tc.Function.Name}
			call.Arguments, call.Raw = parseArguments(tc.Function.Arguments)
			out.ToolCalls = append(out.ToolCalls, call)
		}
	}
	return out, nil
}

// SanitizeBody scrubs an error response that may quote a prefix of the
// submitted API key, and bounds its size so envelopes stay small. Matches
// the OpenAI, OpenRouter and Anthropic error shapes; anything whose lowered
// form mentions a key or a bearer token is replaced whole.
func SanitizeBody(body []byte) string {
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
