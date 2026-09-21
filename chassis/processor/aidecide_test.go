package processor

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/decide"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// decideStub is a controllable decide backend. The decide registry is
// global, so each test registers its stub under its own name.
type decideStub struct {
	name     string
	required []string
	mu       sync.Mutex
	respond  func(ctx context.Context, req decide.Request) (decide.Response, error)
	calls    int32
	sawKey   string
	sawReq   decide.Request
}

func (s *decideStub) Name() string              { return s.name }
func (s *decideStub) DefaultModel() string      { return "stub-model" }
func (s *decideStub) RequiredSecrets() []string { return s.required }
func (s *decideStub) Decide(ctx context.Context, req decide.Request, bag *secrets.SecretBag) (decide.Response, error) {
	atomic.AddInt32(&s.calls, 1)
	s.mu.Lock()
	s.sawReq = req
	if len(s.required) > 0 {
		if v, ok := bag.Get(s.required[0]); ok {
			s.sawKey = string(v)
		}
	}
	respond := s.respond
	s.mu.Unlock()
	return respond(ctx, req)
}

func (s *decideStub) setRespond(f func(ctx context.Context, req decide.Request) (decide.Response, error)) {
	s.mu.Lock()
	s.respond = f
	s.mu.Unlock()
}

func withDecideStub(t *testing.T, name string, stub *decideStub) {
	t.Helper()
	stub.name = name
	decide.Register(name, func(decide.Config) (decide.Backend, error) { return stub, nil })
}

func fp(f float64) *float64 { return &f }

// placeAnswers is a well-formed answer set for placeQuestions.
func placeAnswers(ctx context.Context, req decide.Request) (decide.Response, error) {
	return decide.Response{
		Answers: []decide.Answer{
			{Key: "wake", Type: decide.TypeNoul, Probability: fp(0.9)},
			{Key: "folder", Type: decide.TypeChoice, Choice: "Invoices", Confidence: fp(0.87),
				Probabilities: map[string]float64{"Contracts": 0.04, "Invoices": 0.91, "Correspondence": 0.05}},
		},
		ResolvedModel: "stub-model@1", InputTokens: 120, OutputTokens: 4,
		Cost: fp(0.000005), GenerationID: "gen_1", LatencyMS: 180,
	}, nil
}

const placeQuestions = `{
  "folder": {"type":"choice","instructions":"Which folder?",
             "criteria":{"Contracts":"signed agreements","Invoices":"bills","Correspondence":"letters"}},
  "wake":   {"type":"noul","instructions":"Should the task wake?"}
}`

func aiDecideOp(meta string) operation.Operation {
	return operation.Operation{
		Stack: "site", Scope: 100, Name: "d",
		Resonator: &resonator.Resonator{Exec: "ai://decide"},
		Input:     `{}`,
		Meta:      meta,
	}
}

func TestDecodeDecideWith(t *testing.T) {
	w, into, err := decodeDecideWith(`{"state":"doc","questions":` + placeQuestions + `,"model":"m","provider":"p","intent":"i"}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if into != decideDefaultInto || w.model != "m" || w.provider != "p" || w.intent != "i" || len(w.questions) != 2 {
		t.Fatalf("decoded = %+v into=%q", w, into)
	}
	if string(w.state) != `"doc"` {
		t.Fatalf("state = %s", w.state)
	}

	if _, into, _ := decodeDecideWith(`{"into":"_place.result","state":"x","questions":` + placeQuestions + `}`); into != "_place.result" {
		t.Errorf("custom into = %q", into)
	}
	// A reserved target is refused, and the refusal reports at the default.
	for _, bad := range []string{"_txc.tenant", "@tenant", "_txc"} {
		_, into, err := decodeDecideWith(`{"into":"` + bad + `","state":"x","questions":` + placeQuestions + `}`)
		if err == nil || into != decideDefaultInto {
			t.Errorf("into %q: err=%v into=%q, want refusal at %q", bad, err, into, decideDefaultInto)
		}
	}
	// A malformed request still reports at the author's target.
	if _, into, err := decodeDecideWith(`{"into":"_place","questions":` + placeQuestions + `}`); err == nil || into != "_place" {
		t.Errorf("missing state: err=%v into=%q", err, into)
	}
	if _, _, err := decodeDecideWith(``); err == nil {
		t.Error("empty WITH: want error")
	}
}

func TestExecAIDecideSuccessEnvelope(t *testing.T) {
	stub := &decideStub{required: []string{"DECIDE_TEST_KEY"}, respond: placeAnswers}
	withDecideStub(t, "td-ok", stub)
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("DECIDE_TEST_KEY", "env-key-decide")

	pl, err := pu.ExecAI(context.Background(), aiDecideOp(
		`{"provider":"td-ok","into":"_place","intent":"pony_ingest_place","state":{"title":"Invoice 42"},"questions":`+placeQuestions+`}`))
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	r := gjson.Get(pl.Raw, "_place")
	if !r.Get("ok").Bool() || r.Get("error").Exists() {
		t.Fatalf("want ok result: %s", pl.Raw)
	}
	if gjson.Get(pl.Raw, "_decide").Exists() {
		t.Errorf("default target written despite WITH into: %s", pl.Raw)
	}
	// Answers in question order; P(choice) derived from the distribution;
	// the distribution in the author's option order.
	var keys []string
	r.Get("answers").ForEach(func(k, _ gjson.Result) bool { keys = append(keys, k.String()); return true })
	if strings.Join(keys, ",") != "folder,wake" {
		t.Errorf("answer order = %v, want question order", keys)
	}
	if r.Get("answers.folder.choice").String() != "Invoices" || r.Get("answers.folder.probability").Float() != 0.91 ||
		r.Get("answers.folder.confidence").Float() != 0.87 {
		t.Errorf("folder = %s", r.Get("answers.folder").Raw)
	}
	var opts []string
	r.Get("answers.folder.probabilities").ForEach(func(k, _ gjson.Result) bool { opts = append(opts, k.String()); return true })
	if strings.Join(opts, ",") != "Contracts,Invoices,Correspondence" {
		t.Errorf("distribution order = %v, want option order", opts)
	}
	if r.Get("answers.wake.type").String() != "noul" || r.Get("answers.wake.probability").Float() != 0.9 {
		t.Errorf("wake = %s", r.Get("answers.wake").Raw)
	}
	if r.Get("answers.wake.choice").Exists() || r.Get("answers.wake.probabilities").Exists() || r.Get("answers.wake.confidence").Exists() {
		t.Errorf("noul answer carries fields it was never given: %s", r.Get("answers.wake").Raw)
	}
	if r.Get("provider").String() != "td-ok" || r.Get("model").String() != "stub-model" {
		t.Errorf("provider/model = %s / %s", r.Get("provider"), r.Get("model"))
	}
	if r.Get("usage.input_tokens").Int() != 120 || r.Get("cost").Float() != 0.000005 ||
		r.Get("generation_id").String() != "gen_1" || r.Get("latency_ms").Int() != 180 {
		t.Errorf("metadata = %s", r.Raw)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawKey != "env-key-decide" {
		t.Errorf("backend saw key %q", stub.sawKey)
	}
	if stub.sawReq.Model != "stub-model" || stub.sawReq.Intent != "pony_ingest_place" ||
		gjson.GetBytes(stub.sawReq.State, "title").String() != "Invoice 42" {
		t.Errorf("request = %+v", stub.sawReq)
	}
	if strings.Contains(pl.Raw, "env-key-decide") {
		t.Errorf("secret leaked into the envelope: %s", pl.Raw)
	}
}

func TestExecAIDecideInvalidWithMakesNoCall(t *testing.T) {
	stub := &decideStub{respond: placeAnswers}
	withDecideStub(t, "td-invalid", stub)
	pu, _ := newTestUnit(t)

	pl, err := pu.ExecAI(context.Background(), aiDecideOp(
		`{"provider":"td-invalid","state":"x","questions":{"folder":{"type":"choice","instructions":"Which?","options":{"a":"b"}}}}`))
	if err != nil {
		t.Fatalf("ExecAI returned a Go error (the op would be dropped): %v", err)
	}
	if gjson.Get(pl.Raw, "_decide.ok").Bool() || gjson.Get(pl.Raw, "_decide.error.code").String() != "txco_decide_invalid_with" {
		t.Errorf("want invalid_with at _decide: %s", pl.Raw)
	}
	if !strings.Contains(gjson.Get(pl.Raw, "_decide.error.message").String(), `unknown field "options"`) {
		t.Errorf("message should name the typo: %s", pl.Raw)
	}
	if n := atomic.LoadInt32(&stub.calls); n != 0 {
		t.Errorf("backend called %d times for an invalid request", n)
	}
}

func TestExecAIDecideRefusedIntoWritesNothingReserved(t *testing.T) {
	stub := &decideStub{respond: placeAnswers}
	withDecideStub(t, "td-into", stub)
	pu, _ := newTestUnit(t)

	pl, _ := pu.ExecAI(context.Background(), aiDecideOp(
		`{"provider":"td-into","into":"_txc.tenant","state":"x","questions":`+placeQuestions+`}`))
	if gjson.Get(pl.Raw, "_txc").Exists() {
		t.Fatalf("decide wrote under _txc: %s", pl.Raw)
	}
	if gjson.Get(pl.Raw, "_decide.error.code").String() != "txco_decide_invalid_with" {
		t.Errorf("want refusal at _decide: %s", pl.Raw)
	}
	if n := atomic.LoadInt32(&stub.calls); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

func TestExecAIDecideMissingSecret(t *testing.T) {
	stub := &decideStub{required: []string{"DECIDE_ABSENT_KEY"}, respond: placeAnswers}
	withDecideStub(t, "td-nokey", stub)
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = false

	pl, _ := pu.ExecAI(context.Background(), aiDecideOp(`{"provider":"td-nokey","state":"x","questions":`+placeQuestions+`}`))
	if gjson.Get(pl.Raw, "_decide.error.code").String() != "txco_decide_missing_secret" {
		t.Errorf("want missing_secret: %s", pl.Raw)
	}
	if n := atomic.LoadInt32(&stub.calls); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

func TestExecAIDecideUnknownProvider(t *testing.T) {
	pu, _ := newTestUnit(t)
	pl, err := pu.ExecAI(context.Background(), aiDecideOp(`{"provider":"td-nope","state":"x","questions":`+placeQuestions+`}`))
	if err != nil || gjson.Get(pl.Raw, "_decide.error.code").String() != "txco_decide_no_backend" {
		t.Errorf("want no_backend (err=%v): %s", err, pl.Raw)
	}
}

func TestExecAIDecideFailuresCarryNoAnswers(t *testing.T) {
	cases := map[string]struct {
		respond func(ctx context.Context, req decide.Request) (decide.Response, error)
		code    string
	}{
		"provider http": {
			respond: func(context.Context, decide.Request) (decide.Response, error) {
				return decide.Response{LatencyMS: 12}, &decide.ProviderHTTPError{StatusCode: 503, Body: "overloaded"}
			},
			code: "txco_decide_provider_http",
		},
		"choice not offered": {
			respond: func(ctx context.Context, req decide.Request) (decide.Response, error) {
				resp, _ := placeAnswers(ctx, req)
				resp.Answers[1].Choice = "Receipts"
				return resp, nil
			},
			code: "txco_decide_invalid_answer",
		},
		"question left unanswered": {
			respond: func(ctx context.Context, req decide.Request) (decide.Response, error) {
				resp, _ := placeAnswers(ctx, req)
				resp.Answers = resp.Answers[1:]
				return resp, nil
			},
			code: "txco_decide_invalid_answer",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &decideStub{respond: tc.respond}
			withDecideStub(t, "td-fail-"+strings.ReplaceAll(name, " ", "-"), stub)
			pu, _ := newTestUnit(t)
			pl, err := pu.ExecAI(context.Background(), aiDecideOp(
				`{"provider":"`+stub.name+`","state":"x","questions":`+placeQuestions+`}`))
			if err != nil {
				t.Fatalf("ExecAI: %v", err)
			}
			r := gjson.Get(pl.Raw, "_decide")
			if r.Get("ok").Bool() || r.Get("error.code").String() != tc.code {
				t.Errorf("want %s: %s", tc.code, pl.Raw)
			}
			if r.Get("answers").Exists() {
				t.Errorf("a failed decide must carry no answers: %s", pl.Raw)
			}
		})
	}
}

func TestExecAIDecideDeadlineIsTimeout(t *testing.T) {
	stub := &decideStub{respond: func(ctx context.Context, req decide.Request) (decide.Response, error) {
		<-ctx.Done()
		return decide.Response{}, &decide.ProviderNetError{Reason: ctx.Err().Error()}
	}}
	withDecideStub(t, "td-slow", stub)
	pu, _ := newTestUnit(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	pl, _ := pu.ExecAI(ctx, aiDecideOp(`{"provider":"td-slow","state":"x","questions":`+placeQuestions+`}`))
	if gjson.Get(pl.Raw, "_decide.error.code").String() != "txco_decide_timeout" {
		t.Errorf("want txco_decide_timeout: %s", pl.Raw)
	}
}

// TestDecideStackPolicyInWhen is the end-to-end claim of the primitive: a
// stack reads the judgment in ordinary WHEN clauses at the next scope.
//
// It also pins two WHEN behaviors every decide call site depends on (WHEN
// compares through gjson's coercion, resonator.compareTyped):
//
//   - A MISSING path compares as 0. `probability > x` stays quiet when the
//     decide failed, but `probability < x` FIRES — so every rule gates on
//     `<into>.ok == true` first.
//   - An INTEGER literal compares via Int(), truncating the probability:
//     `probability > 0` is false for 0.91. Write thresholds as decimals.
//
// If either assertion flips, WHEN semantics changed: update docs/ai.md.
func TestDecideStackPolicyInWhen(t *testing.T) {
	stub := &decideStub{respond: placeAnswers}
	withDecideStub(t, "td-stack", stub)
	pu, _ := newTestUnit(t)

	stack := "decidetest/app"
	seedNamedOp(t, pu, stack, 0, "place",
		`WHEN .go == true WITH provider = "td-stack", state = .doc, questions = .q, into = "_place" EXEC "ai://decide"`)
	seedNamedOp(t, pu, stack, 1, "file",
		`WHEN ._place.ok == true && ._place.answers.folder.choice == "Invoices" && ._place.answers.folder.probability > 0.8 EMIT .placed = "Invoices"`)
	seedNamedOp(t, pu, stack, 1, "fallback",
		`WHEN ._place.ok != true EMIT .placed = "top"`)
	seedNamedOp(t, pu, stack, 1, "skip_unguarded",
		`WHEN ._place.answers.wake.probability < 0.2 EMIT .skip_unguarded = true`)
	seedNamedOp(t, pu, stack, 1, "skip_guarded",
		`WHEN ._place.ok == true && ._place.answers.wake.probability < 0.2 EMIT .skip_guarded = true`)
	seedNamedOp(t, pu, stack, 1, "int_literal",
		`WHEN ._place.ok == true && ._place.answers.folder.probability > 0 EMIT .int_literal = true`)

	body := `{"go":true,"doc":{"title":"Invoice 42"},"q":` + placeQuestions + `}`

	// Decide succeeds: policy files the document; nothing skips.
	out := runStage(t, context.Background(), pu, body, stack+"/0")
	if got := gjson.Get(out, "placed").String(); got != "Invoices" {
		t.Errorf("success: placed = %q, want Invoices: %s", got, out)
	}
	if gjson.Get(out, "skip_unguarded").Exists() || gjson.Get(out, "skip_guarded").Exists() {
		t.Errorf("success: wake 0.9 must not skip: %s", out)
	}
	if gjson.Get(out, "int_literal").Exists() {
		t.Errorf("`probability > 0` fired — integer literals no longer truncate; update docs/ai.md: %s", out)
	}

	// Decide fails: the fallback lane runs, and the unguarded `<` rule
	// fires on the missing path while the guarded one does not.
	stub.setRespond(func(context.Context, decide.Request) (decide.Response, error) {
		return decide.Response{}, &decide.ProviderHTTPError{StatusCode: 503, Body: "down"}
	})
	out = runStage(t, context.Background(), pu, body, stack+"/0")
	if got := gjson.Get(out, "placed").String(); got != "top" {
		t.Errorf("failure: placed = %q, want the fallback: %s", got, out)
	}
	if !gjson.Get(out, "skip_unguarded").Bool() {
		t.Errorf("failure: the unguarded `< 0.2` rule did not fire — missing paths no longer compare as 0; update docs/ai.md: %s", out)
	}
	if gjson.Get(out, "skip_guarded").Exists() {
		t.Errorf("failure: the ok-guarded rule fired: %s", out)
	}
}

// TestDecideMockedStack: a stack test mocks ai://decide by pattern, and the
// mock is written in the NORMALIZED result shape (what the op merges), not
// the provider's wire shape — the result shape is the contract stacks see.
// A sentinel backend proves no provider is reached.
func TestDecideMockedStack(t *testing.T) {
	decide.Register("td-sentinel", func(decide.Config) (decide.Backend, error) {
		t.Fatal("a mocked ai://decide must not open a backend")
		return nil, nil
	})
	pu, _ := newTestUnit(t)
	stack := "decidemock/app"
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		stack, 0, "place",
		`WHEN .go == true WITH provider = "td-sentinel", state = "x", questions = .q, into = "_place" EXEC "ai://decide"`,
		`{"_place":{"ok":true,"answers":{"folder":{"type":"choice","choice":"Invoices","probability":0.93,"probabilities":{"Invoices":0.93,"Contracts":0.07}}}}}`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedNamedOp(t, pu, stack, 1, "file",
		`WHEN ._place.ok == true && ._place.answers.folder.probability > 0.8 EMIT .placed = ._place.answers.folder.choice`)

	out := runStage(t, context.Background(), pu,
		`{"go":true,"q":`+placeQuestions+`,"_txc":{"mocks":["decidemock/app/**"]}}`, stack+"/0")
	if got := gjson.Get(out, "placed").String(); got != "Invoices" {
		t.Errorf("placed = %q, want Invoices from the mock: %s", got, out)
	}
}

// TestDecideDocsExample runs the docs/ai.md example as written (plus a
// provider override, since the default backend is not linked in this test
// binary): nested &object questions, then the policy and fallback rules.
func TestDecideDocsExample(t *testing.T) {
	stub := &decideStub{respond: func(ctx context.Context, req decide.Request) (decide.Response, error) {
		return decide.Response{Answers: []decide.Answer{
			{Key: "folder", Type: decide.TypeChoice, Choice: "Invoices",
				Probabilities: map[string]float64{"Invoices": 0.91, "Contracts": 0.09}},
			{Key: "automated", Type: decide.TypeNoul, Probability: fp(0.04)},
		}}, nil
	}}
	withDecideStub(t, "td-docs", stub)
	pu, _ := newTestUnit(t)

	stack := "decidedocs/app"
	seedNamedOp(t, pu, stack, 0, "place", `WHEN ._doc.ready == true
WITH state     = ._doc.summary,
     questions = &object(
       "folder",    &object("type", "choice",
                            "instructions", "Which folder does this document belong in?",
                            "criteria", &object("Invoices",  "bills and receipts",
                                                "Contracts", "signed agreements")),
       "automated", &object("type", "noul",
                            "instructions", "Was this sent by a machine rather than a person?")),
     into      = "_place",
     timeout   = 5000,
     provider  = "td-docs"
EXEC "ai://decide"`)
	seedNamedOp(t, pu, stack, 1, "file", `WHEN ._place.ok == true && ._place.answers.folder.choice == "Invoices"
     && ._place.answers.folder.probability > 0.80
EMIT .folder = "Invoices"`)
	seedNamedOp(t, pu, stack, 1, "fallback", `WHEN ._place.ok != true
EMIT .folder = "Inbox"`)

	out := runStage(t, context.Background(), pu, `{"_doc":{"ready":true,"summary":"Invoice #42 from Acme, due Oct 1"}}`, stack+"/0")
	if got := gjson.Get(out, "folder").String(); got != "Invoices" {
		t.Fatalf("folder = %q, want Invoices: %s", got, out)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	// &object does not keep key order, so find the question by key.
	var folder *decide.Question
	for i := range stub.sawReq.Questions {
		if stub.sawReq.Questions[i].Key == "folder" {
			folder = &stub.sawReq.Questions[i]
		}
	}
	if len(stub.sawReq.Questions) != 2 || folder == nil || len(folder.Choices) != 2 ||
		string(stub.sawReq.State) != `"Invoice #42 from Acme, due Oct 1"` {
		t.Errorf("request from the docs rule = %+v", stub.sawReq)
	}
}
