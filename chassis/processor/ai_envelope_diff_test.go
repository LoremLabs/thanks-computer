package processor

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/embed"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// Frozen copies of the pre-jsonx sjson chains. The converted builders
// must reproduce these byte-for-byte for every input combination.

func buildChatResponseEnvelopeFrozen(resp chat.Response, runErr error, schemaStatus string, validatedPayload json.RawMessage, routing string) string {
	raw := "{}"
	if runErr == nil {
		raw, _ = sjson.Set(raw, "text", resp.Text)
		if len(validatedPayload) > 0 {
			raw, _ = sjson.SetRaw(raw, "schema_validated_payload", string(validatedPayload))
		}
	} else {
		errBody := map[string]any{"message": runErr.Error()}
		if coded, ok := runErr.(chat.CodedError); ok {
			errBody["code"] = coded.Code()
		} else {
			errBody["code"] = "txco_chat_unknown"
		}
		raw, _ = sjson.Set(raw, "chat.error", errBody)
	}
	raw, _ = sjson.Set(raw, "_txc.chat.provider", resp.Provider)
	raw, _ = sjson.Set(raw, "_txc.chat.model", resp.Model)
	raw, _ = sjson.Set(raw, "_txc.chat.tokens.in", resp.TokensIn)
	raw, _ = sjson.Set(raw, "_txc.chat.tokens.out", resp.TokensOut)
	raw, _ = sjson.Set(raw, "_txc.chat.latency_ms", resp.LatencyMS)
	raw, _ = sjson.Set(raw, "_txc.chat.retries", resp.Retries)
	if routing != "" {
		raw, _ = sjson.Set(raw, "_txc.chat.routing_decision", routing)
	}
	if schemaStatus != "" {
		raw, _ = sjson.Set(raw, "_txc.chat.schema_validation", schemaStatus)
	}
	return raw
}

func buildEmbedResponseEnvelopeFrozen(resp embed.Response, runErr error, routing string, single bool) string {
	raw := "{}"
	if runErr == nil {
		vecsJSON, _ := json.Marshal(resp.Vectors)
		raw, _ = sjson.SetRaw(raw, "_embed.vectors", string(vecsJSON))
		if single && len(resp.Vectors) > 0 {
			v0JSON, _ := json.Marshal(resp.Vectors[0])
			raw, _ = sjson.SetRaw(raw, "_embed.vector", string(v0JSON))
		}
	} else {
		errBody := map[string]any{"message": runErr.Error()}
		if coded, ok := runErr.(embed.CodedError); ok {
			errBody["code"] = coded.Code()
		} else {
			errBody["code"] = "txco_embed_unknown"
		}
		raw, _ = sjson.Set(raw, "embed.error", errBody)
	}
	raw, _ = sjson.Set(raw, "_embed.provider", resp.Provider)
	raw, _ = sjson.Set(raw, "_embed.model", resp.Model)
	raw, _ = sjson.Set(raw, "_embed.dimensions", resp.Dimensions)
	raw, _ = sjson.Set(raw, "_embed.tokens", resp.Tokens)
	raw, _ = sjson.Set(raw, "_embed.latency_ms", resp.LatencyMS)
	raw, _ = sjson.Set(raw, "_embed.retries", resp.Retries)
	if routing != "" {
		raw, _ = sjson.Set(raw, "_embed.routing_decision", routing)
	}
	return raw
}

func TestChatEnvelopeMatchesFrozen(t *testing.T) {
	resps := []chat.Response{
		{},
		{Text: "hello <world> é🎈", Provider: "openrouter", Model: "gpt-5.4-mini", TokensIn: 120, TokensOut: 48, LatencyMS: 900, Retries: 1},
		{Text: `line"quote\`, Provider: "openai", Model: "m", LatencyMS: 0},
	}
	errs := []error{nil, errors.New("boom"), &chat.SchemaFailedError{Reason: "nope"}}
	payloads := []json.RawMessage{nil, json.RawMessage(`{"k":1}`)}
	for _, resp := range resps {
		for _, e := range errs {
			for _, vp := range payloads {
				for _, routing := range []string{"", "explicit-provider"} {
					for _, ss := range []string{"", schemaValidationOK} {
						want := buildChatResponseEnvelopeFrozen(resp, e, ss, vp, routing)
						got := buildChatResponseEnvelope(resp, e, ss, vp, routing)
						if got != want {
							t.Fatalf("chat envelope mismatch\nwant %q\ngot  %q", want, got)
						}
					}
				}
			}
		}
	}
}

func TestEmbedEnvelopeMatchesFrozen(t *testing.T) {
	resps := []embed.Response{
		{},
		{Vectors: [][]float32{{0.1, -0.5}, {1.25, 3}}, Provider: "openai", Model: "text-embedding-3-small", Dimensions: 2, Tokens: 12, LatencyMS: 40, Retries: 0},
	}
	errs := []error{nil, errors.New("boom")}
	for _, resp := range resps {
		for _, e := range errs {
			for _, routing := range []string{"", "default"} {
				for _, single := range []bool{false, true} {
					want := buildEmbedResponseEnvelopeFrozen(resp, e, routing, single)
					got := buildEmbedResponseEnvelope(resp, e, routing, single)
					if got != want {
						t.Fatalf("embed envelope mismatch\nwant %q\ngot  %q", want, got)
					}
				}
			}
		}
	}
}

func TestChatErrorPayloadMatchesFrozen(t *testing.T) {
	op := operation.Operation{Meta: `{"m":1}`}
	frozen := func(providerName, model, routing string, err error) string {
		raw := "{}"
		errBody := map[string]any{"message": err.Error()}
		if coded, ok := err.(chat.CodedError); ok {
			errBody["code"] = coded.Code()
		} else {
			errBody["code"] = "txco_chat_unknown"
		}
		raw, _ = sjson.Set(raw, "chat.error", errBody)
		if providerName != "" {
			raw, _ = sjson.Set(raw, "_txc.chat.provider", providerName)
		}
		if model != "" {
			raw, _ = sjson.Set(raw, "_txc.chat.model", model)
		}
		if routing != "" {
			raw, _ = sjson.Set(raw, "_txc.chat.routing_decision", routing)
		}
		return raw
	}
	for _, p := range []string{"", "prov"} {
		for _, m := range []string{"", "model-x"} {
			for _, r := range []string{"", "routed"} {
				for _, e := range []error{errors.New("plain"), &chat.SchemaFailedError{Reason: "r"}} {
					want := frozen(p, m, r, e)
					got := chatErrorPayload(op, p, m, r, e).Raw
					if got != want {
						t.Fatalf("chatErrorPayload mismatch\nwant %q\ngot  %q", want, got)
					}
				}
			}
		}
	}
}

func TestEmbedErrorPayloadMatchesFrozen(t *testing.T) {
	op := operation.Operation{Meta: `{"m":1}`}
	frozen := func(providerName, routing string, err error) string {
		raw := "{}"
		errBody := map[string]any{"message": err.Error()}
		if coded, ok := err.(embed.CodedError); ok {
			errBody["code"] = coded.Code()
		} else {
			errBody["code"] = "txco_embed_unknown"
		}
		raw, _ = sjson.Set(raw, "embed.error", errBody)
		if providerName != "" {
			raw, _ = sjson.Set(raw, "_embed.provider", providerName)
		}
		if routing != "" {
			raw, _ = sjson.Set(raw, "_embed.routing_decision", routing)
		}
		return raw
	}
	for _, p := range []string{"", "prov"} {
		for _, r := range []string{"", "routed"} {
			e := errors.New("boom")
			want := frozen(p, r, e)
			got := embedErrorPayload(op, p, r, e).Raw
			if got != want {
				t.Fatalf("embedErrorPayload mismatch\nwant %q\ngot  %q", want, got)
			}
		}
	}
}
