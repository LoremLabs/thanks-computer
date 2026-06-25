package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// aiSchemePrefix is the EXEC-value prefix this handler claims.
const aiSchemePrefix = "ai://"

// aiSubOpChat is the only sub-op v1 dispatches. Future ai://embed,
// ai://transcribe, ai://image slot in here.
const aiSubOpChat = "chat"

// Schema validation outcome labels. Surfaced on the trace event and on
// the response envelope's _txc.chat.schema_validation field. v1 is
// binary; the "repaired" case (provider auto-coerces, e.g. OpenAI
// strict mode) is intentionally not distinguished — see the chat-exec
// spec §13.
const (
	schemaValidationOK     = "ok"
	schemaValidationFailed = "failed"
)

// ExecAI dispatches ai://<sub_op>. v1 supports only ai://chat.
//
// The handler:
//
//  1. Parses the sub-op (chat is the only one wired today).
//  2. Decodes op.Meta (WITH-clause materialization) into a chat.Request.
//  3. Resolves a backend via chat.Resolve (provider override or
//     first-registered default; capability matching is its own follow-up).
//  4. Materializes the backend's RequiredSecrets() through the per-tenant
//     store with optional env-var fallback (AIChatEnvFallback config),
//     reusing the existing chassis pattern: SecretBag, RecordSecretMaterialize,
//     fuelCostSecretMaterialize fuel charge.
//  5. Renders {{@path}} prompt templates over op.Input.
//  6. Calls backend.Run exactly once (no tool loop in v1).
//  7. Validates the response against WITH schema if present (binary
//     ok / failed; no repair semantics).
//  8. Emits a chat.completion trace event with provider, model, token
//     counts, routing decision, retries, latency, and schema validation
//     outcome.
//  9. Builds the response envelope: top-level text + schema_validated_payload
//     + chat.error (when set) + _txc.chat.* metadata. Token counts are
//     recorded HERE for downstream observability/billing; they are NOT
//     charged to the chassis fuel meter — provider compute is a separate
//     dimension.
//
// Error policy: chat-level failures (auth, provider HTTP/net, schema
// validation, missing secret, no backend) surface as a structured
// `chat.error` field on the response envelope so rule authors can handle
// uniformly via `WHEN @chat.error EXEC ...`. ExecAI returns a non-nil
// error only for malformed-shape conditions a rule author cannot work
// around (unrecognized sub-op, op.Meta failing JSON parse, an unset
// runtime dependency). This gives rule authors one error-handling
// pattern; structured codes (txco_chat_*) live on the envelope.
func (pu *Unit) ExecAI(ctx context.Context, op operation.Operation) (event.Payload, error) {
	subOp, err := parseAISubOp(op.Resonator.Exec)
	if err != nil {
		return chatErrorPayload(op, "", "", "", err), err
	}
	switch subOp {
	case aiSubOpEmbed:
		return pu.execEmbed(ctx, op)
	case aiSubOpChat:
		// fall through to the chat path below
	default:
		uerr := &chat.UnsupportedSubOpError{SubOp: subOp}
		return chatErrorPayload(op, "", "", "", uerr), uerr
	}

	// Decode WITH clause into request shape + control fields. op.Meta
	// is already-materialized JSON from the WITH-handling path at
	// processor.go:2714 — string templates ({{@field}}) are passed
	// through verbatim and rendered below.
	withCfg, err := decodeChatWith(op.Meta)
	if err != nil {
		// Bad WITH shape is the rule author's bug; surface on envelope
		// AND error so the test signal is clear.
		return chatErrorPayload(op, "", "", "", err), err
	}

	// Build chat.Config (the chassis-owned http.Client routes through
	// the configured egress.Guard).
	cfg := chat.Config{HTTPClient: pu.HTTPClient}

	backend, routingDecision, err := chat.Resolve(withCfg.provider, cfg)
	if err != nil {
		return chatErrorPayload(op, "", "", routingDecision, err), nil
	}

	// Materialize backend-declared secrets (in addition to any
	// WITH-declared secrets the existing materialize loop at
	// processor.go:632 already placed in op.Secrets). Pass by pointer
	// so the lazy-alloc inside SecretBag.Set lands on ExecAI's bag,
	// not on a helper's local copy.
	if err := pu.materializeChatSecrets(ctx, &op, backend); err != nil {
		return chatErrorPayload(op, backend.Name(), "", routingDecision, err), nil
	}

	// Render prompt templates over op.Input. WITH `messages = [...]` opts
	// out of templating entirely — the message bodies are passed verbatim
	// to the provider.
	if withCfg.useMessages {
		// messages take precedence; no template rendering
	} else if withCfg.prompt != "" {
		rendered, rerr := chat.Render(withCfg.prompt, []byte(op.Input), pu.Logger)
		if rerr != nil {
			return chatErrorPayload(op, backend.Name(), "", routingDecision, rerr), rerr
		}
		withCfg.prompt = rendered
		if withCfg.system != "" {
			renderedSys, rerr2 := chat.Render(withCfg.system, []byte(op.Input), pu.Logger)
			if rerr2 != nil {
				return chatErrorPayload(op, backend.Name(), "", routingDecision, rerr2), rerr2
			}
			withCfg.system = renderedSys
		}
	}

	req := chat.Request{
		Messages: buildMessages(withCfg),
		Schema:   withCfg.schema,
		Model:    withCfg.model,
		Limits:   withCfg.limits,
		Intent:   withCfg.intent,
	}

	resp, runErr := backend.Run(ctx, req, &op.Secrets)
	// Provider errors don't fail the Exec — rule author handles via
	// WHEN @chat.error EXEC. Carry through resp partial fields
	// (LatencyMS, Retries) for observability even on failure.

	// Schema validation (binary ok / failed in v1).
	schemaStatus := ""
	var validatedPayload json.RawMessage
	if len(withCfg.schema) > 0 && runErr == nil {
		validated, verr := validateChatSchema(resp.Text, withCfg.schema)
		if verr != nil {
			runErr = &chat.SchemaFailedError{Reason: verr.Error()}
			schemaStatus = schemaValidationFailed
		} else {
			schemaStatus = schemaValidationOK
			validatedPayload = validated
		}
	}

	// Trace event — open-ended Fields per trace.TimelineEvent contract.
	emitChatCompletionEvent(ctx, backend.Name(), resp, routingDecision, schemaStatus, runErr)

	// Build response envelope.
	raw := buildChatResponseEnvelope(resp, runErr, schemaStatus, validatedPayload, routingDecision)
	// Stamp the chassis-wide _txc_op_debug field when the rule opted
	// in. The chassis post-Exec strip in processor.go consumes this
	// field BEFORE the envelope merge, emits it as an op.debug trace
	// event, and ensures rules never see debug content. The stamped
	// fields here are deliberately conservative: only source data
	// (rendered prompt, outbound messages, model) — never headers,
	// never bag values, never raw provider bodies that may quote keys.
	if withCfg.debug {
		raw = stampChatOpDebug(raw, withCfg, req, resp, runErr)
	}
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}, nil
}

// parseAISubOp pulls the sub-op token off the ai:// prefix. Strips any
// trailing path / query (reserved for future use).
func parseAISubOp(opName string) (string, error) {
	if !strings.HasPrefix(opName, aiSchemePrefix) {
		return "", fmt.Errorf("chat: ExecAI received non-ai:// EXEC %q", opName)
	}
	rest := strings.TrimPrefix(opName, aiSchemePrefix)
	// Cut at the first / or ? to leave room for ai://chat/something later.
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' || rest[i] == '?' {
			return rest[:i], nil
		}
	}
	return rest, nil
}

// chatWith is the decoded WITH clause for ai://chat. Templated string
// fields (prompt, system) are passed through verbatim until the renderer
// runs over op.Input.
type chatWith struct {
	prompt      string
	system      string
	useMessages bool
	messages    []chat.Message
	schema      json.RawMessage
	model       string
	provider    string
	intent      string
	limits      chat.Limits
	debug       bool // WITH debug = true → stamp _txc_op_debug; chassis surfaces to trace
}

// decodeChatWith reads op.Meta (JSON produced by the chassis's WITH
// materialization path) into a chatWith. Validates the prompt/messages
// XOR rule.
func decodeChatWith(meta string) (chatWith, error) {
	if meta == "" {
		return chatWith{}, &chat.InvalidWithError{
			Reason: "WITH clause is empty; supply prompt or messages",
		}
	}

	out := chatWith{}

	if r := gjson.Get(meta, "prompt"); r.Exists() {
		out.prompt = r.String()
	}
	if r := gjson.Get(meta, "system"); r.Exists() {
		out.system = r.String()
	}
	if r := gjson.Get(meta, "messages"); r.Exists() {
		out.useMessages = true
		if err := json.Unmarshal([]byte(r.Raw), &out.messages); err != nil {
			return out, &chat.InvalidWithError{
				Reason: "failed to parse WITH messages array: " + err.Error(),
			}
		}
	}
	if r := gjson.Get(meta, "schema"); r.Exists() {
		out.schema = json.RawMessage(r.Raw)
	}
	if r := gjson.Get(meta, "model"); r.Exists() {
		out.model = r.String()
	}
	if r := gjson.Get(meta, "provider"); r.Exists() {
		out.provider = r.String()
	}
	if r := gjson.Get(meta, "intent"); r.Exists() {
		out.intent = r.String()
	}
	if r := gjson.Get(meta, "limits.timeout_ms"); r.Exists() {
		out.limits.TimeoutMs = int(r.Int())
	}
	if r := gjson.Get(meta, "limits.max_cost_usd"); r.Exists() {
		out.limits.MaxCostUSD = r.Float()
	}
	if r := gjson.Get(meta, "debug"); r.Exists() {
		out.debug = r.Bool()
	}

	// XOR validation: exactly one of prompt / messages must be present.
	hasPrompt := out.prompt != ""
	if hasPrompt && out.useMessages {
		return out, &chat.InvalidWithError{
			Reason: "specify one of WITH prompt OR WITH messages, not both",
			Detail: map[string]string{"prompt_len": strconv.Itoa(len(out.prompt))},
		}
	}
	if !hasPrompt && !out.useMessages {
		return out, &chat.InvalidWithError{
			Reason: "WITH must specify either prompt or messages",
		}
	}

	return out, nil
}

// buildMessages constructs the message array fed to the provider.
// Messages-form passes through verbatim; prompt-form builds [system?, user].
func buildMessages(w chatWith) []chat.Message {
	if w.useMessages {
		return w.messages
	}
	var out []chat.Message
	if w.system != "" {
		out = append(out, chat.Message{Role: "system", Content: w.system})
	}
	out = append(out, chat.Message{Role: "user", Content: w.prompt})
	return out
}

// materializeChatSecrets walks Backend.RequiredSecrets() and ensures
// each name is present in op.Secrets. Lookup chain:
//
//  1. Skip if already in op.Secrets (a WITH-declared override resolved
//     by the existing materialize loop at processor.go:632 wins).
//  2. Per-tenant secret store via pu.Secrets.MaterializeForOpSlug.
//  3. (When Conf.AIChatEnvFallback is true) os.Getenv(name).
//  4. Hard miss → MissingSecretError.
//
// Mirrors the existing materialize loop's bookkeeping: per-name fuel
// charge (100 fuel), audit counter increment. Cleanup of cleartext is
// already deferred by the existing loop's `defer op.Secrets.Zero()` at
// processor.go:680 — runs on every Run exit path, including this one.
func (pu *Unit) materializeChatSecrets(ctx context.Context, op *operation.Operation, backend chat.Backend) error {
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
			return &chat.MissingSecretError{Backend: backend.Name(), Secret: name}
		}
		op.Secrets.Set(name, cleartext)
		if pu.Mc != nil {
			pu.Mc.RecordSecretMaterialize(ctx, tenantSlug, name)
		}
		_ = addFuel(ctx, fuelCostSecretMaterialize, op.Stack+"/"+strconv.Itoa(op.Scope))
	}
	return nil
}

// lookupChatSecret implements the tenant → env fallback chain. Returns
// secrets.ErrSecretNotFound when both miss.
func (pu *Unit) lookupChatSecret(ctx context.Context, op operation.Operation, tenantSlug, name string) ([]byte, error) {
	if pu.Secrets != nil && tenantSlug != "" {
		cleartext, _, err := pu.Secrets.MaterializeForOpSlug(ctx, tenantSlug, op.Stack, name)
		if err == nil {
			return cleartext, nil
		}
		// Anything other than "not found" is a real error: store down,
		// crypto failure, etc. Surface to caller.
		if err != secrets.ErrSecretNotFound {
			return nil, err
		}
	}
	if pu.Conf.AIChatEnvFallback {
		if val := os.Getenv(name); val != "" {
			return []byte(val), nil
		}
	}
	return nil, secrets.ErrSecretNotFound
}

// validateChatSchema parses text as JSON and validates against schema.
// Returns the validated raw bytes (re-marshalled compact) or an error.
func validateChatSchema(text string, schemaJSON json.RawMessage) (json.RawMessage, error) {
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return nil, fmt.Errorf("response is not valid JSON: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("inline://chat-schema", strings.NewReader(string(schemaJSON))); err != nil {
		return nil, fmt.Errorf("schema compile: %w", err)
	}
	sch, err := compiler.Compile("inline://chat-schema")
	if err != nil {
		return nil, fmt.Errorf("schema compile: %w", err)
	}
	if err := sch.Validate(parsed); err != nil {
		return nil, err
	}
	compact, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("re-marshal validated payload: %w", err)
	}
	return compact, nil
}

// emitChatCompletionEvent writes one chat.completion TimelineEvent.
// Fields are open-ended (map[string]any) per the trace contract. Token
// counts ride this event for downstream observability/billing — but are
// NOT charged to the chassis fuel meter (provider compute is a separate
// dimension; see chassis/processor/budget.go's fuel cost rationale).
func emitChatCompletionEvent(ctx context.Context, providerName string, resp chat.Response, routing, schemaStatus string, runErr error) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	fields := map[string]any{
		"provider":         providerName,
		"model":            resp.Model,
		"routing_decision": routing,
		"tokens_in":        resp.TokensIn,
		"tokens_out":       resp.TokensOut,
		"latency_ms":       resp.LatencyMS,
		"retries":          resp.Retries,
	}
	if schemaStatus != "" {
		fields["schema_validation"] = schemaStatus
	}
	if runErr != nil {
		if coded, ok := runErr.(chat.CodedError); ok {
			fields["error_code"] = coded.Code()
		} else {
			fields["error_code"] = "txco_chat_unknown"
		}
	}
	tr.Event(trace.TimelineEvent{
		Ts:     time.Now(),
		Event:  "chat.completion",
		Fields: fields,
	})
}

// buildChatResponseEnvelope assembles the JSON envelope returned by the
// EXEC. Shape per the chat-exec spec:
//
//	{
//	  "text": "...",                              // present on success
//	  "schema_validated_payload": {...},          // present when schema set + ok
//	  "chat": {"error": {code, message, ...}},    // present on failure
//	  "_txc": {
//	    "chat": {
//	      "provider", "model",
//	      "tokens": {"in", "out"},
//	      "latency_ms", "retries",
//	      "routing_decision", "schema_validation"
//	    }
//	  }
//	}
//
// `chat.error` is top-level (not under _txc) so rule authors can dispatch
// uniformly with `WHEN @chat.error EXEC ...`.
func buildChatResponseEnvelope(resp chat.Response, runErr error, schemaStatus string, validatedPayload json.RawMessage, routing string) string {
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

	// Metadata always present, even on error, so the trace + envelope
	// agree.
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

// stampChatOpDebug attaches `_txc_op_debug` to raw with chat-flavored
// fields. The chassis strip + trace emit picks this up and surfaces it
// to the timeline as an `op.debug` event; the field never reaches the
// envelope merge so rules cannot accidentally come to depend on it.
//
// Fields populated:
//
//   - rendered_prompt: the prompt string after {{@path}} template
//     substitution (the field that would have caught the smoke-time
//     "blank prompt to LLM" bug in one look). Absent in messages-form.
//   - system_rendered: the rendered system message (if any).
//   - messages_sent: the full outbound messages array as sent to the
//     provider.
//   - model_sent: the resolved model identifier.
//   - intent: the trace label authors stamp via WITH intent.
//   - schema_present: whether the call requested structured output.
//   - error_code: when the call failed, the txco_chat_* code.
//
// Notably NOT populated: outbound HTTP headers (Authorization carries
// the secret); raw provider response body (provider 401s may quote a
// key prefix — sanitization lives in the backend, not here, and adding
// raw bodies would require API surface on chat.Backend we're not ready
// to commit to in v1). These are obvious v1.1 extensions if the
// smoke-feedback loop demands them.
func stampChatOpDebug(raw string, cfg chatWith, req chat.Request, resp chat.Response, runErr error) string {
	rendered := ""
	if !cfg.useMessages {
		// After template render the prompt sits in cfg.prompt.
		rendered = cfg.prompt
	}

	msgsBytes, _ := json.Marshal(req.Messages)
	debugObj := map[string]any{
		"rendered_prompt": rendered,
		"system_rendered": cfg.system,
		"messages_sent":   json.RawMessage(msgsBytes),
		"model_sent":      req.Model,
		"intent":          req.Intent,
		"schema_present":  len(req.Schema) > 0,
	}
	if runErr != nil {
		if coded, ok := runErr.(chat.CodedError); ok {
			debugObj["error_code"] = coded.Code()
		} else {
			debugObj["error_code"] = "txco_chat_unknown"
		}
	}

	debugBytes, err := json.Marshal(debugObj)
	if err != nil {
		// Defensive — debug should never break the real response path.
		return raw
	}
	updated, err := sjson.SetRaw(raw, opDebugField, string(debugBytes))
	if err != nil {
		return raw
	}
	return updated
}

// chatErrorPayload is a small helper for ExecAI's pre-Run error paths
// (sub-op recognition, decode-WITH failure, secret materialization
// failure, no backend). Same shape buildChatResponseEnvelope produces
// but without a successful Response to draw metadata from.
func chatErrorPayload(op operation.Operation, providerName, model, routing string, err error) event.Payload {
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
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}
}

// Silence unused-import linter when we're not yet referencing certain
// imports in some build configurations.
var _ = operation.MetaFromContext
var _ = zap.String
