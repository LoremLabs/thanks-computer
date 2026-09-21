// Package vercel is the ai://decide backend for Vercel AI Gateway's
// evaluation API: POST https://ai-gateway.vercel.sh/v1/evaluate, documented
// at https://vercel.com/docs/ai-gateway/modalities/evaluation. The default
// model is TypeSafe AI's Jev (`typesafe-ai/jev`).
//
// Wire translation is the only thing this package knows. The gateway calls a
// noul question `boolean` and reports `{type:"boolean", probability}`; the
// chassis contract says `noul` everywhere else, so the rename happens here in
// both directions and nowhere in txcl. Choice and score answers also carry a
// `confidence` on the live wire (verified 2026-09-21; not yet in Vercel's
// docs), which passes through as decide.Answer.Confidence.
//
// The cleartext key (VERCEL_AI_KEY) is read from the secrets.SecretBag
// once per call, placed in the Authorization header, and never stored,
// logged, or traced. Error bodies that look key-related are replaced
// wholesale.
package vercel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/decide"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const (
	defaultModel    = "typesafe-ai/jev"
	defaultEndpoint = "https://ai-gateway.vercel.sh/v1/evaluate"
	secretName      = "VERCEL_AI_KEY"

	// maxChoices is Jev's documented choice cardinality. Checked before the
	// request so an oversized question costs nothing.
	maxChoices = 255

	// maxErrorBody bounds the provider error text carried onto the envelope.
	maxErrorBody = 1024
)

func init() {
	decide.Register("vercel", func(cfg decide.Config) (decide.Backend, error) {
		if cfg.HTTPClient == nil {
			return nil, errors.New("vercel: nil HTTPClient (decide.Config must carry the chassis-owned client)")
		}
		return &backend{httpClient: cfg.HTTPClient, endpoint: defaultEndpoint, zdr: cfg.ZeroDataRetention}, nil
	})
}

type backend struct {
	httpClient *http.Client
	endpoint   string
	zdr        bool
}

func (b *backend) Name() string              { return "vercel" }
func (b *backend) DefaultModel() string      { return defaultModel }
func (b *backend) RequiredSecrets() []string { return []string{secretName} }

// Decide sends every question in one /v1/evaluate call. Retries once on
// transient failures (HTTP 5xx, network); never on 4xx.
func (b *backend) Decide(ctx context.Context, req decide.Request, bag *secrets.SecretBag) (decide.Response, error) {
	model := req.Model
	if model == "" {
		model = defaultModel
	}
	partial := decide.Response{Provider: b.Name(), Model: model}

	for _, q := range req.Questions {
		if q.Type == decide.TypeChoice && len(q.Choices) > maxChoices {
			return partial, &decide.InvalidWithError{Reason: fmt.Sprintf(
				"question %q offers %d choices; the provider accepts at most %d", q.Key, len(q.Choices), maxChoices)}
		}
	}
	if bag == nil {
		return partial, &decide.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}
	apiKey, ok := bag.Get(secretName)
	if !ok || len(apiKey) == 0 {
		return partial, &decide.MissingSecretError{Backend: b.Name(), Secret: secretName}
	}

	body, err := buildRequestBody(req, model, b.zdr)
	if err != nil {
		return partial, fmt.Errorf("vercel: build request: %w", err)
	}

	start := time.Now()
	resp, retries, runErr := b.doWithSimpleRetry(ctx, apiKey, body)
	partial.LatencyMS = time.Since(start).Milliseconds()
	partial.Retries = retries
	if runErr != nil {
		return partial, runErr
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	partial.LatencyMS = time.Since(start).Milliseconds()
	if resp.StatusCode >= 400 {
		return partial, &decide.ProviderHTTPError{StatusCode: resp.StatusCode, Body: errorMessage(respBody)}
	}
	return parseResponse(respBody, partial)
}

func (b *backend) doWithSimpleRetry(ctx context.Context, apiKey, body []byte) (*http.Response, int, error) {
	attempt := func() (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		// Cleartext touches only this header.Set; never stored/logged. The
		// handler's bag.Zero() wipes the underlying bytes afterwards.
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
	if ctx.Err() != nil {
		return nil, 0, &decide.ProviderNetError{Reason: ctx.Err().Error()}
	}
	select {
	case <-time.After(250 * time.Millisecond):
	case <-ctx.Done():
		return nil, 0, &decide.ProviderNetError{Reason: ctx.Err().Error()}
	}

	resp2, err2 := attempt()
	if err2 != nil {
		return nil, 1, &decide.ProviderNetError{Reason: err2.Error()}
	}
	return resp2, 1, nil
}

// buildRequestBody writes the /v1/evaluate request. Questions and choice
// options keep author order (Go maps would sort them), so the body is built
// by hand rather than marshalled from a map.
func buildRequestBody(req decide.Request, model string, zdr bool) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"model":`)
	writeJSON(&buf, model)
	buf.WriteString(`,"state":`)
	if !json.Valid(req.State) {
		return nil, errors.New("state is not valid JSON")
	}
	buf.Write(req.State)
	buf.WriteString(`,"questions":{`)
	for i, q := range req.Questions {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSON(&buf, q.Key)
		buf.WriteString(`:{"type":`)
		writeJSON(&buf, wireType(q.Type))
		buf.WriteString(`,"instructions":`)
		writeJSON(&buf, q.Instructions)
		switch q.Type {
		case decide.TypeNoul:
			if q.True != "" || q.False != "" {
				buf.WriteString(`,"criteria":{`)
				sep := ""
				if q.True != "" {
					buf.WriteString(`"true":`)
					writeJSON(&buf, q.True)
					sep = ","
				}
				if q.False != "" {
					buf.WriteString(sep + `"false":`)
					writeJSON(&buf, q.False)
				}
				buf.WriteByte('}')
			}
		case decide.TypeChoice:
			buf.WriteString(`,"criteria":{`)
			for j, o := range q.Choices {
				if j > 0 {
					buf.WriteByte(',')
				}
				writeJSON(&buf, o.Name)
				buf.WriteByte(':')
				writeJSON(&buf, o.Description)
			}
			buf.WriteByte('}')
		case decide.TypeScore:
			buf.WriteString(`,"criteria":`)
			writeJSON(&buf, q.Levels)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('}')
	if zdr {
		buf.WriteString(`,"providerOptions":{"gateway":{"zeroDataRetention":true}}`)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSON(buf *bytes.Buffer, v any) {
	b, _ := json.Marshal(v) // strings and []string cannot fail
	buf.Write(b)
}

// wireType maps a contract question type to the gateway's name for it.
func wireType(t string) string {
	if t == decide.TypeNoul {
		return "boolean"
	}
	return t
}

// contractType maps a gateway answer type back to the contract's name.
func contractType(t string) string {
	if t == "boolean" {
		return decide.TypeNoul
	}
	return t
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Probability   *float64           `json:"probability"`
	Choice        *string            `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int64 `json:"inputTokens"`
		OutputTokens int64 `json:"outputTokens"`
	} `json:"usage"`
	ProviderMetadata struct {
		Gateway struct {
			Cost         json.RawMessage `json:"cost"`
			GenerationID string          `json:"generationId"`
		} `json:"gateway"`
	} `json:"providerMetadata"`
}

// parseResponse translates the gateway's answers as reported. Whether they
// satisfy the questions (all answered, choices offered, probabilities in
// range) is decide.Normalize's job, applied by the handler.
func parseResponse(body []byte, partial decide.Response) (decide.Response, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return partial, &decide.ProviderParseError{Reason: "empty body"}
	}
	var raw wireResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return partial, &decide.ProviderParseError{Reason: err.Error(), BodyLen: len(body)}
	}
	if raw.Answers == nil {
		return partial, &decide.ProviderParseError{Reason: "no answers in response", BodyLen: len(body)}
	}

	out := partial
	out.ResolvedModel = raw.Model
	out.InputTokens = raw.Usage.InputTokens
	out.OutputTokens = raw.Usage.OutputTokens
	out.GenerationID = raw.ProviderMetadata.Gateway.GenerationID
	out.Cost = parseCost(raw.ProviderMetadata.Gateway.Cost)
	for key, wa := range raw.Answers {
		a := decide.Answer{
			Key:           key,
			Type:          contractType(wa.Type),
			Probability:   wa.Probability,
			Score:         wa.Score,
			Probabilities: wa.Probabilities,
			Confidence:    wa.Confidence,
		}
		if wa.Choice != nil {
			a.Choice = *wa.Choice
		}
		out.Answers = append(out.Answers, a)
	}
	return out, nil
}

// parseCost reads the gateway's cost, which is documented as a decimal
// string ("0.00001155"); a bare number is accepted too. Anything else is
// treated as unreported.
func parseCost(raw json.RawMessage) *float64 {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return &f
		}
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return &f
	}
	return nil
}

// errorMessage extracts a human-readable message from a gateway error body
// (`{"error":{"message"}}` or `{"message","error_type"}`), falling back to
// the raw body. Key-related text is replaced wholesale — providers
// occasionally echo a key prefix — and the result is bounded.
func errorMessage(body []byte) string {
	var shaped struct {
		Message   string `json:"message"`
		ErrorType string `json:"error_type"`
		Error     *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &shaped) == nil {
		switch {
		case shaped.Error != nil && shaped.Error.Message != "":
			msg = shaped.Error.Message
			if shaped.Error.Type != "" {
				msg = shaped.Error.Type + ": " + msg
			}
		case shaped.Message != "":
			msg = shaped.Message
			if shaped.ErrorType != "" {
				msg = shaped.ErrorType + ": " + msg
			}
		}
	}
	low := strings.ToLower(msg)
	for _, marker := range []string{"api key", "api_key", "apikey", "bearer ", "unauthorized", "authentication", "oidc"} {
		if strings.Contains(low, marker) {
			return "authentication or key-related provider error (body sanitized)"
		}
	}
	if len(msg) > maxErrorBody {
		msg = strings.ToValidUTF8(msg[:maxErrorBody], "") + "...(truncated)"
	}
	return msg
}
