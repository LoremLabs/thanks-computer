package processor

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/embed"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// aiSubOpEmbed is the ai:// sub-op this handler claims.
const aiSubOpEmbed = "embed"

// execEmbed dispatches ai://embed. It mirrors ExecAI's chat path but for
// embeddings: decode the WITH clause, resolve a backend, materialize any
// declared secrets (the ollama backend declares none), call Embed once, and
// build the response envelope under `_embed` (an underscore key — vectors are
// large and should not leak into the default web-response projection;
// downstream ops still read them via `@_embed.vectors`).
//
// Error policy matches chat: provider-level failures surface as a structured
// top-level `embed.error` so authors handle them with `WHEN @embed.error`.
// execEmbed returns a non-nil error only for malformed-shape conditions a
// rule author cannot work around (bad WITH).
func (pu *Unit) execEmbed(ctx context.Context, op operation.Operation) (event.Payload, error) {
	withCfg, err := decodeEmbedWith(op.Meta)
	if err != nil {
		return embedErrorPayload(op, "", "", err), err
	}

	cfg := embed.Config{HTTPClient: pu.HTTPClient, OllamaBaseURL: pu.Conf.EmbedOllamaBaseURL}
	backend, routingDecision, err := embed.Resolve(withCfg.provider, cfg)
	if err != nil {
		return embedErrorPayload(op, "", routingDecision, err), nil
	}

	if err := pu.materializeEmbedSecrets(ctx, &op, backend); err != nil {
		return embedErrorPayload(op, backend.Name(), routingDecision, err), nil
	}

	req := embed.Request{
		Texts:      withCfg.texts,
		Model:      withCfg.model,
		Dimensions: withCfg.dimensions,
		Intent:     withCfg.intent,
	}
	resp, runErr := backend.Embed(ctx, req, &op.Secrets)

	emitEmbedCompletionEvent(ctx, backend.Name(), resp, routingDecision, runErr)

	raw := buildEmbedResponseEnvelope(resp, runErr, routingDecision, withCfg.single)
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}, nil
}

// embedWith is the decoded WITH clause for ai://embed.
type embedWith struct {
	texts      []string
	single     bool // true when the author passed WITH text (vs texts); drives `_embed.vector`
	model      string
	provider   string
	dimensions int
	intent     string
}

// decodeEmbedWith reads op.Meta (JSON produced by the WITH materialization
// path) into an embedWith. Exactly one of `text` / `texts` must be present.
func decodeEmbedWith(meta string) (embedWith, error) {
	if meta == "" {
		return embedWith{}, &embed.InvalidWithError{Reason: "WITH clause is empty; supply text or texts"}
	}
	out := embedWith{}

	hasText := false
	if r := gjson.Get(meta, "text"); r.Exists() {
		hasText = true
		out.single = true
		out.texts = []string{r.String()}
	}
	if r := gjson.Get(meta, "texts"); r.Exists() {
		if hasText {
			return out, &embed.InvalidWithError{Reason: "specify one of WITH text OR WITH texts, not both"}
		}
		if !r.IsArray() {
			return out, &embed.InvalidWithError{Reason: "WITH texts must be an array of strings"}
		}
		var texts []string
		if err := json.Unmarshal([]byte(r.Raw), &texts); err != nil {
			return out, &embed.InvalidWithError{Reason: "failed to parse WITH texts array: " + err.Error()}
		}
		out.texts = texts
	}
	if len(out.texts) == 0 {
		return out, &embed.InvalidWithError{Reason: "WITH must specify text or a non-empty texts array"}
	}

	if r := gjson.Get(meta, "model"); r.Exists() {
		out.model = r.String()
	}
	if r := gjson.Get(meta, "provider"); r.Exists() {
		out.provider = r.String()
	}
	if r := gjson.Get(meta, "dimensions"); r.Exists() {
		out.dimensions = int(r.Int())
	}
	if r := gjson.Get(meta, "intent"); r.Exists() {
		out.intent = r.String()
	}
	return out, nil
}

// materializeEmbedSecrets walks Backend.RequiredSecrets() and ensures each
// name is present in op.Secrets. Reuses the chat handler's tenant→env lookup
// chain (lookupChatSecret) and the same per-secret fuel charge + audit
// counter. A backend with no required secrets (ollama) is a no-op.
func (pu *Unit) materializeEmbedSecrets(ctx context.Context, op *operation.Operation, backend embed.Backend) error {
	required := backend.RequiredSecrets()
	if len(required) == 0 {
		return nil
	}
	tenantSlug := tenantScope(ctx)
	for _, name := range required {
		if _, already := op.Secrets.Get(name); already {
			continue
		}
		cleartext, err := pu.lookupChatSecret(ctx, *op, tenantSlug, name)
		if err != nil {
			return &embed.MissingSecretError{Backend: backend.Name(), Secret: name}
		}
		op.Secrets.Set(name, cleartext)
		if pu.Mc != nil {
			pu.Mc.RecordSecretMaterialize(ctx, tenantSlug, name)
		}
		_ = addFuel(ctx, fuelCostSecretMaterialize, op.Stack+"/"+strconv.Itoa(op.Scope))
	}
	return nil
}

// emitEmbedCompletionEvent writes one embed.completion TimelineEvent (a
// file-trace diagnostic; the NATS sink ships only summary fields, so no
// sink change is needed). Token counts ride the event for observability but
// are NOT charged to fuel — provider compute is a separate dimension.
func emitEmbedCompletionEvent(ctx context.Context, providerName string, resp embed.Response, routing string, runErr error) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	fields := map[string]any{
		"provider":         providerName,
		"model":            resp.Model,
		"routing_decision": routing,
		"count":            len(resp.Vectors),
		"dimensions":       resp.Dimensions,
		"tokens":           resp.Tokens,
		"latency_ms":       resp.LatencyMS,
		"retries":          resp.Retries,
	}
	if runErr != nil {
		if coded, ok := runErr.(embed.CodedError); ok {
			fields["error_code"] = coded.Code()
		} else {
			fields["error_code"] = "txco_embed_unknown"
		}
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "embed.completion", Fields: fields})
}

// buildEmbedResponseEnvelope assembles the JSON envelope returned by the EXEC:
//
//	{
//	  "embed": {"error": {code, message}},     // present on failure
//	  "_embed": {
//	    "vectors": [[...], ...],                // always on success
//	    "vector":  [...],                       // success + single (WITH text)
//	    "provider", "model", "dimensions",
//	    "tokens", "latency_ms", "retries"
//	  }
//	}
//
// `embed.error` is top-level (not under `_embed`) so rule authors dispatch
// uniformly with `WHEN @embed.error EXEC ...`. Vectors live under the
// underscore-prefixed `_embed` so they don't leak into the default web
// projection while remaining readable by downstream ops (`@_embed.vectors`).
func buildEmbedResponseEnvelope(resp embed.Response, runErr error, routing string, single bool) string {
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

// embedErrorPayload builds the envelope for execEmbed's pre-Embed error paths
// (decode-WITH failure, no backend, secret materialization failure).
func embedErrorPayload(op operation.Operation, providerName, routing string, err error) event.Payload {
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
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}
}
