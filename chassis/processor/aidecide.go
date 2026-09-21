package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/decide"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcguard"
)

// aiSubOpDecide is the ai:// sub-op this handler claims.
const aiSubOpDecide = "decide"

// decideDefaultInto is where the result lands when WITH into is absent.
// `_`-prefixed, so it never reaches a web body on its own.
const decideDefaultInto = "_decide"

// execDecide dispatches ai://decide: state plus bounded, typed questions in,
// probabilistic judgments out. It decodes the WITH clause, validates it
// (nothing is billed for a malformed request), resolves a backend,
// materializes its secrets, asks once, and enforces the answer contract
// (decide.Normalize) before anything reaches the envelope.
//
// The result lands under `WITH into` (default `_decide`):
//
//	<into>.ok             true | false
//	<into>.answers.<key>  one typed answer per question (only when ok)
//	<into>.error          {code, message} (only when not ok)
//	<into>.provider / .model / .usage / .cost / .generation_id / .latency_ms
//
// Every failure — bad WITH, no backend, missing secret, provider error,
// timeout, an answer that breaks the contract — is data: `ok = false` plus a
// coded error, and a nil Go error so the op still merges. The op makes no
// policy decision and never turns a failure into an answer; what a failure
// MEANS (fail open, fail closed, fall back, ask a human) is the stack's
// WHEN clause. Stacks should gate on `<into>.ok == true` before reading a
// probability: a missing path compares as 0.
//
// ai:// is a trusted transport, so the output merges unsanitized — which is
// why the author-chosen `into` goes through txcguard.AuthorTarget: it may
// not name a reserved `_txc` path.
func (pu *Unit) execDecide(ctx context.Context, op operation.Operation) (event.Payload, error) {
	w, into, err := decodeDecideWith(op.Meta)
	if err != nil {
		emitDecideCompletionEvent(ctx, "", "", w, decide.Response{}, err)
		return decidePayload(op, into, w.questions, decide.Response{Model: w.model}, err), nil
	}

	cfg := decide.Config{HTTPClient: pu.HTTPClient, ZeroDataRetention: pu.Conf.DecideZeroDataRetention}
	backend, routing, err := decide.Resolve(w.provider, cfg)
	if err != nil {
		emitDecideCompletionEvent(ctx, "", routing, w, decide.Response{}, err)
		return decidePayload(op, into, w.questions, decide.Response{Model: w.model}, err), nil
	}
	if w.model == "" {
		w.model = backend.DefaultModel()
	}
	base := decide.Response{Provider: backend.Name(), Model: w.model}

	if err := pu.materializeDecideSecrets(ctx, &op, backend); err != nil {
		emitDecideCompletionEvent(ctx, backend.Name(), routing, w, base, err)
		return decidePayload(op, into, w.questions, base, err), nil
	}
	// This frame owns the handler-materialized cleartext (see execEmbed).
	defer op.Secrets.Zero()

	req := decide.Request{
		State:     w.state,
		Questions: w.questions,
		Model:     w.model,
		Intent:    w.intent,
	}
	resp, runErr := backend.Decide(ctx, req, &op.Secrets)
	if resp.Provider == "" {
		resp.Provider = backend.Name()
	}
	if resp.Model == "" {
		resp.Model = w.model
	}
	if runErr == nil {
		runErr = decide.Normalize(req, &resp)
	}
	// Whatever the backend reported, a passed op deadline is a timeout —
	// one code a stack can dispatch on, whichever backend ran.
	if runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		runErr = &decide.TimeoutError{Reason: runErr.Error()}
	}

	emitDecideCompletionEvent(ctx, backend.Name(), routing, w, resp, runErr)
	return decidePayload(op, into, w.questions, resp, runErr), nil
}

// decideWith is the decoded WITH clause for ai://decide. `timeout` is read
// by the dispatch loop for every op and needs nothing here.
type decideWith struct {
	state     json.RawMessage
	questions []decide.Question
	model     string
	provider  string
	intent    string
}

// decodeDecideWith reads op.Meta into a decideWith and resolves the output
// target. The target is resolved first so that even a malformed request
// reports its error where the author will look for it; a refused target
// reports at the default.
func decodeDecideWith(meta string) (decideWith, string, error) {
	if meta == "" {
		return decideWith{}, decideDefaultInto, &decide.InvalidWithError{Reason: "WITH clause is empty; supply state and questions"}
	}
	f := gjson.GetMany(meta, "into", "state", "questions", "model", "provider", "intent")

	into := decideDefaultInto
	if f[0].Exists() {
		target, ok := txcguard.AuthorTarget(f[0].String())
		if !ok || target == "" {
			return decideWith{}, decideDefaultInto, &decide.InvalidWithError{Reason: "WITH into must be a plain envelope path outside _txc"}
		}
		into = target
	}

	w := decideWith{model: f[3].String(), provider: f[4].String(), intent: f[5].String()}
	state, err := decide.ParseState(f[1])
	if err != nil {
		return w, into, err
	}
	w.state = state
	questions, err := decide.ParseQuestions(f[2])
	if err != nil {
		return w, into, err
	}
	w.questions = questions
	return w, into, nil
}

// materializeDecideSecrets walks Backend.RequiredSecrets() and ensures each
// name is present in op.Secrets — the same tenant→env lookup chain
// (lookupChatSecret), fuel charge and audit counter as chat and embed.
func (pu *Unit) materializeDecideSecrets(ctx context.Context, op *operation.Operation, backend decide.Backend) error {
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
			return &decide.MissingSecretError{Backend: backend.Name(), Secret: name}
		}
		op.Secrets.Set(name, cleartext)
		if pu.Mc != nil {
			pu.Mc.RecordSecretMaterialize(ctx, tenantSlug, name)
		}
		_ = addFuel(ctx, fuelCostSecretMaterialize, op.Stack+"/"+strconv.Itoa(op.Scope))
	}
	return nil
}

// emitDecideCompletionEvent writes one decide.completion TimelineEvent.
// Counts and labels only: never the state, the questions, or the answers.
// Token counts are NOT charged to fuel — provider compute is a separate
// dimension (the ai://embed convention).
func emitDecideCompletionEvent(ctx context.Context, providerName, routing string, w decideWith, resp decide.Response, runErr error) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	fields := map[string]any{
		"provider":       providerName,
		"model":          resp.Model,
		"question_count": len(w.questions),
		"input_tokens":   resp.InputTokens,
		"output_tokens":  resp.OutputTokens,
		"latency_ms":     resp.LatencyMS,
		"retries":        resp.Retries,
	}
	if routing != "" {
		fields["routing_decision"] = routing
	}
	if w.intent != "" {
		fields["intent"] = w.intent
	}
	if resp.ResolvedModel != "" {
		fields["resolved_model"] = resp.ResolvedModel
	}
	if resp.Cost != nil {
		fields["provider_cost"] = *resp.Cost
	}
	if resp.GenerationID != "" {
		fields["generation_id"] = resp.GenerationID
	}
	if runErr != nil {
		fields["error_code"] = decideErrorCode(runErr)
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "decide.completion", Fields: fields})
}

func decideErrorCode(err error) string {
	var coded decide.CodedError
	if errors.As(err, &coded) {
		return coded.Code()
	}
	return "txco_decide_unknown"
}

// decidePayload builds the op output: the result object set at `into`.
// Answers appear only when runErr is nil — never a partial set.
func decidePayload(op operation.Operation, into string, questions []decide.Question, resp decide.Response, runErr error) event.Payload {
	result := string(buildDecideResult(questions, resp, runErr))
	raw, err := sjson.SetRaw(`{}`, into, result)
	if err != nil {
		// into passed AuthorTarget, so this is unreachable in practice; land
		// at the default target (`_decide`) rather than lose the result.
		raw = `{"` + decideDefaultInto + `":` + result + `}`
	}
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}
}

// buildDecideResult writes the result object by hand so answers keep
// question order and distributions keep option / level order (a Go map
// would sort them).
func buildDecideResult(questions []decide.Question, resp decide.Response, runErr error) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"ok":`)
	if runErr == nil {
		buf.WriteString(`true,"answers":{`)
		for i, a := range resp.Answers {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONValue(&buf, a.Key)
			buf.WriteByte(':')
			writeDecideAnswer(&buf, a, distributionOrder(questions, a))
		}
		buf.WriteByte('}')
	} else {
		buf.WriteString(`false,"error":{"code":`)
		writeJSONValue(&buf, decideErrorCode(runErr))
		buf.WriteString(`,"message":`)
		writeJSONValue(&buf, runErr.Error())
		buf.WriteByte('}')
	}
	if resp.Provider != "" {
		buf.WriteString(`,"provider":`)
		writeJSONValue(&buf, resp.Provider)
	}
	if resp.Model != "" {
		buf.WriteString(`,"model":`)
		writeJSONValue(&buf, resp.Model)
	}
	if resp.InputTokens > 0 || resp.OutputTokens > 0 {
		buf.WriteString(`,"usage":{"input_tokens":`)
		writeJSONValue(&buf, resp.InputTokens)
		buf.WriteString(`,"output_tokens":`)
		writeJSONValue(&buf, resp.OutputTokens)
		buf.WriteByte('}')
	}
	if resp.Cost != nil {
		buf.WriteString(`,"cost":`)
		writeJSONValue(&buf, *resp.Cost)
	}
	if resp.GenerationID != "" {
		buf.WriteString(`,"generation_id":`)
		writeJSONValue(&buf, resp.GenerationID)
	}
	if resp.LatencyMS > 0 {
		buf.WriteString(`,"latency_ms":`)
		writeJSONValue(&buf, resp.LatencyMS)
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func writeDecideAnswer(buf *bytes.Buffer, a decide.Answer, order []string) {
	buf.WriteString(`{"type":`)
	writeJSONValue(buf, a.Type)
	if a.Type == decide.TypeChoice {
		buf.WriteString(`,"choice":`)
		writeJSONValue(buf, a.Choice)
	}
	if a.Score != nil {
		buf.WriteString(`,"score":`)
		writeJSONValue(buf, *a.Score)
	}
	if a.Probability != nil {
		buf.WriteString(`,"probability":`)
		writeJSONValue(buf, *a.Probability)
	}
	if a.Confidence != nil {
		buf.WriteString(`,"confidence":`)
		writeJSONValue(buf, *a.Confidence)
	}
	if len(a.Probabilities) > 0 {
		buf.WriteString(`,"probabilities":{`)
		n := 0
		for _, k := range order {
			p, ok := a.Probabilities[k]
			if !ok {
				continue
			}
			if n > 0 {
				buf.WriteByte(',')
			}
			n++
			writeJSONValue(buf, k)
			buf.WriteByte(':')
			writeJSONValue(buf, p)
		}
		buf.WriteByte('}')
	}
	buf.WriteByte('}')
}

// distributionOrder is the output order for an answer's distribution: a
// choice's options as the author listed them, a score's levels by index.
// (Normalize has already reduced the distribution to those keys.)
func distributionOrder(questions []decide.Question, a decide.Answer) []string {
	for _, q := range questions {
		if q.Key != a.Key {
			continue
		}
		var order []string
		switch q.Type {
		case decide.TypeChoice:
			for _, o := range q.Choices {
				order = append(order, o.Name)
			}
		case decide.TypeScore:
			for i := range q.Levels {
				order = append(order, strconv.Itoa(i))
			}
		}
		return order
	}
	return nil
}

func writeJSONValue(buf *bytes.Buffer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// Only non-finite floats can fail, and Normalize rejects those.
		buf.WriteString("null")
		return
	}
	buf.Write(b)
}
