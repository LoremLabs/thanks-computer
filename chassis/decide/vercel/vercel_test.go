package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/decide"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

func bagWithKey(key string) *secrets.SecretBag {
	var bag secrets.SecretBag
	bag.Set(secretName, []byte(key))
	return &bag
}

func request(t *testing.T, questions string) decide.Request {
	t.Helper()
	qs, err := decide.ParseQuestions(gjson.Parse(questions))
	if err != nil {
		t.Fatalf("ParseQuestions: %v", err)
	}
	return decide.Request{State: json.RawMessage(`"My card was charged twice for one order."`), Questions: qs}
}

// server answers every call with status + body and records what it saw.
type server struct {
	calls   int32
	auth    string
	body    string
	handler func(w http.ResponseWriter, r *http.Request)
}

func newServer(t *testing.T, status int, body string) (*server, *backend) {
	t.Helper()
	s := &server{}
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.calls, 1)
		s.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		s.body = string(b)
		s.handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return s, &backend{httpClient: ts.Client(), endpoint: ts.URL}
}

const allTypes = `{
  "automated": {"type":"noul","instructions":"Sent by a machine?","criteria":{"true":"newsletters","false":"a person"}},
  "route":     {"type":"choice","instructions":"Route this ticket.",
                "criteria":{"billing":"payment problems","shipping":"delivery problems","technical":"bugs"}},
  "urgency":   {"type":"score","instructions":"How urgent?","criteria":["low","medium","high"]}
}`

// The response shape documented at
// https://vercel.com/docs/ai-gateway/modalities/evaluation (HTTP API), plus
// what the live wire adds (captured 2026-09-21): a `confidence` on choice and
// score answers, mirrored under providerMetadata.typesafe. Probabilities come
// back in the provider's order, not the author's.
const allTypesResponse = `{
  "model": "typesafe-ai/jev",
  "answers": {
    "automated": {"type":"boolean","probability":0.02},
    "route":     {"type":"choice","choice":"billing","probabilities":{"technical":0.02,"billing":0.97,"shipping":0.01},"confidence":0.95},
    "urgency":   {"type":"score","score":1.3,"probabilities":{"0":0.1,"1":0.5,"2":0.4},"confidence":0.61}
  },
  "usage": {"inputTokens": 275, "outputTokens": 20},
  "providerMetadata": {
    "typesafe": {"confidence": {"route": 0.95, "urgency": 0.61}},
    "gateway": {
      "routing": {"originalModelId":"typesafe-ai/jev","resolvedProvider":"typesafe-ai","finalProvider":"typesafe-ai"},
      "cost": "0.00001155", "marketCost": "0.00001155", "generationId": "gen_abc123"
    }
  }
}`

func TestDecideAllTypes(t *testing.T) {
	s, b := newServer(t, 200, allTypesResponse)
	req := request(t, allTypes)
	resp, err := b.Decide(context.Background(), req, bagWithKey("vck_test"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if s.auth != "Bearer vck_test" {
		t.Errorf("Authorization = %q", s.auth)
	}

	// Request: noul travels as boolean; author order survives; criteria per type.
	body := gjson.Parse(s.body)
	if got := body.Get("model").String(); got != defaultModel {
		t.Errorf("model = %q, want default %q", got, defaultModel)
	}
	if got := body.Get("state").String(); got != "My card was charged twice for one order." {
		t.Errorf("state = %q", got)
	}
	if got := body.Get("questions.automated.type").String(); got != "boolean" {
		t.Errorf("noul wire type = %q, want boolean", got)
	}
	if got := body.Get("questions.automated.criteria.false").String(); got != "a person" {
		t.Errorf("noul criteria = %s", body.Get("questions.automated.criteria").Raw)
	}
	var qOrder, oOrder []string
	body.Get("questions").ForEach(func(k, _ gjson.Result) bool { qOrder = append(qOrder, k.String()); return true })
	body.Get("questions.route.criteria").ForEach(func(k, _ gjson.Result) bool { oOrder = append(oOrder, k.String()); return true })
	if strings.Join(qOrder, ",") != "automated,route,urgency" || strings.Join(oOrder, ",") != "billing,shipping,technical" {
		t.Errorf("order: questions=%v options=%v", qOrder, oOrder)
	}
	if got := body.Get("questions.urgency.criteria.2").String(); got != "high" {
		t.Errorf("score criteria = %s", body.Get("questions.urgency.criteria").Raw)
	}
	if body.Get("providerOptions").Exists() {
		t.Errorf("providerOptions sent without zero-data-retention: %s", s.body)
	}

	// Response: boolean comes back as noul; metadata parsed.
	if err := decide.Normalize(req, &resp); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	noul, choice, score := resp.Answers[0], resp.Answers[1], resp.Answers[2]
	if noul.Type != decide.TypeNoul || *noul.Probability != 0.02 || noul.Confidence != nil {
		t.Errorf("noul = %+v (Jev reports no confidence for noul)", noul)
	}
	if choice.Choice != "billing" || *choice.Probability != 0.97 || choice.Confidence == nil || *choice.Confidence != 0.95 {
		t.Errorf("choice = %+v", choice)
	}
	if *score.Score != 1.3 || score.Probabilities["2"] != 0.4 || score.Confidence == nil || *score.Confidence != 0.61 {
		t.Errorf("score = %+v", score)
	}
	if resp.Provider != "vercel" || resp.Model != defaultModel || resp.ResolvedModel != "typesafe-ai/jev" {
		t.Errorf("provider/model = %q %q %q", resp.Provider, resp.Model, resp.ResolvedModel)
	}
	if resp.InputTokens != 275 || resp.OutputTokens != 20 || resp.GenerationID != "gen_abc123" {
		t.Errorf("usage/generation = %d %d %q", resp.InputTokens, resp.OutputTokens, resp.GenerationID)
	}
	if resp.Cost == nil || *resp.Cost != 0.00001155 {
		t.Errorf("cost = %v, want 0.00001155", resp.Cost)
	}
}

func TestDecideSingleNoulAndObjectState(t *testing.T) {
	s, b := newServer(t, 200, `{"model":"typesafe-ai/jev","answers":{"refunded":{"type":"boolean","probability":0.99}},"usage":{"inputTokens":283,"outputTokens":21}}`)
	req := request(t, `{"refunded":{"type":"noul","instructions":"Was a refund issued?"}}`)
	req.State = json.RawMessage(`{"order":{"id":"A-1","status":"refunded"}}`)
	req.Model = "typesafe-ai/jev"
	resp, err := b.Decide(context.Background(), req, bagWithKey("k"))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	body := gjson.Parse(s.body)
	if body.Get("state.order.status").String() != "refunded" {
		t.Errorf("object state not passed through: %s", s.body)
	}
	if body.Get("questions.refunded.criteria").Exists() {
		t.Errorf("noul without criteria must send none: %s", s.body)
	}
	if err := decide.Normalize(req, &resp); err != nil || *resp.Answers[0].Probability != 0.99 {
		t.Fatalf("Normalize: %v %+v", err, resp.Answers)
	}
	if resp.Cost != nil {
		t.Errorf("cost unreported, got %v", *resp.Cost)
	}
}

func TestDecideZeroDataRetention(t *testing.T) {
	s, b := newServer(t, 200, allTypesResponse)
	b.zdr = true
	if _, err := b.Decide(context.Background(), request(t, allTypes), bagWithKey("k")); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if !gjson.Get(s.body, "providerOptions.gateway.zeroDataRetention").Bool() {
		t.Errorf("zeroDataRetention not sent: %s", s.body)
	}
}

func TestDecideProviderFailureRetriesOnce(t *testing.T) {
	s, b := newServer(t, 503, `{"error":{"message":"upstream unavailable","type":"overloaded"}}`)
	resp, err := b.Decide(context.Background(), request(t, allTypes), bagWithKey("k"))
	var he *decide.ProviderHTTPError
	if !errors.As(err, &he) || he.StatusCode != 503 {
		t.Fatalf("err = %v, want ProviderHTTPError 503", err)
	}
	if !strings.Contains(he.Body, "upstream unavailable") {
		t.Errorf("message not extracted: %q", he.Body)
	}
	if got := atomic.LoadInt32(&s.calls); got != 2 || resp.Retries != 1 {
		t.Errorf("calls = %d retries = %d, want 2 / 1", got, resp.Retries)
	}
}

func TestDecideClientErrorNoRetry(t *testing.T) {
	// The gateway's live 400 body (captured 2026-09-21).
	s, b := newServer(t, 400, `{"error":{"message":"questions.a.type: Invalid discriminator value. Expected 'boolean' | 'choice' | 'score'","param":null,"type":"invalid_request_error"}}`)
	_, err := b.Decide(context.Background(), request(t, allTypes), bagWithKey("k"))
	var he *decide.ProviderHTTPError
	if !errors.As(err, &he) || he.StatusCode != 400 {
		t.Fatalf("err = %v, want ProviderHTTPError 400", err)
	}
	if !strings.HasPrefix(he.Body, "invalid_request_error: questions.a.type: Invalid discriminator") {
		t.Errorf("message = %q", he.Body)
	}
	if got := atomic.LoadInt32(&s.calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestDecideAuthErrorSanitized(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"Authentication failed","param":null,"type":"authentication_error"}}`, // live 401
		`{"error":{"message":"Invalid API key vck_abc123...","type":"authentication_error"}}`,
	} {
		_, b := newServer(t, 401, body)
		_, err := b.Decide(context.Background(), request(t, allTypes), bagWithKey("vck_abc123secret"))
		if err == nil || strings.Contains(err.Error(), "vck_abc") || !strings.Contains(err.Error(), "body sanitized") {
			t.Errorf("err = %v, want sanitized auth error", err)
		}
	}
}

func TestDecideTimeout(t *testing.T) {
	s, b := newServer(t, 200, allTypesResponse)
	s.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := b.Decide(ctx, request(t, allTypes), bagWithKey("k"))
	var ne *decide.ProviderNetError
	if !errors.As(err, &ne) {
		t.Fatalf("err = %v, want ProviderNetError (the handler maps a passed deadline to txco_decide_timeout)", err)
	}
	if got := atomic.LoadInt32(&s.calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry after the deadline)", got)
	}
}

func TestDecideMalformedResponse(t *testing.T) {
	for name, body := range map[string]string{
		"empty":      ``,
		"not json":   `<html>502</html>`,
		"no answers": `{"model":"typesafe-ai/jev"}`,
	} {
		_, b := newServer(t, 200, body)
		_, err := b.Decide(context.Background(), request(t, allTypes), bagWithKey("k"))
		var pe *decide.ProviderParseError
		if !errors.As(err, &pe) {
			t.Errorf("%s: err = %v, want ProviderParseError", name, err)
		}
	}
}

func TestDecideMissingKeyAndTooManyChoicesSendNothing(t *testing.T) {
	s, b := newServer(t, 200, allTypesResponse)
	if _, err := b.Decide(context.Background(), request(t, allTypes), &secrets.SecretBag{}); err == nil {
		t.Error("missing key: want error")
	} else if _, ok := err.(*decide.MissingSecretError); !ok {
		t.Errorf("missing key err = %T", err)
	}

	var opts []string
	for i := 0; i < maxChoices+1; i++ {
		opts = append(opts, fmt.Sprintf(`"o%d":""`, i))
	}
	big := request(t, `{"pick":{"type":"choice","instructions":"Pick one","criteria":{`+strings.Join(opts, ",")+`}}}`)
	if _, err := b.Decide(context.Background(), big, bagWithKey("k")); err == nil {
		t.Error("256 choices: want error")
	} else if _, ok := err.(*decide.InvalidWithError); !ok {
		t.Errorf("256 choices err = %T", err)
	}
	if got := atomic.LoadInt32(&s.calls); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}

func TestRegistered(t *testing.T) {
	b, err := decide.Open("vercel", decide.Config{HTTPClient: http.DefaultClient})
	if err != nil || b.Name() != "vercel" || b.DefaultModel() != defaultModel {
		t.Fatalf("Open(vercel) = %v, %v", b, err)
	}
	if _, err := decide.Open("vercel", decide.Config{}); err == nil {
		t.Error("nil HTTPClient: want error")
	}
}

// TestLiveGateway calls the real gateway. It runs only with
// VERCEL_AI_KEY set (and TXCO_LIVE_DECIDE=1, so a stray key in the
// environment never makes the default test run billable).
func TestLiveGateway(t *testing.T) {
	key := os.Getenv(secretName)
	if key == "" || os.Getenv("TXCO_LIVE_DECIDE") != "1" {
		t.Skip("set VERCEL_AI_KEY and TXCO_LIVE_DECIDE=1 to call the real gateway")
	}
	b := &backend{httpClient: &http.Client{Timeout: 20 * time.Second}, endpoint: defaultEndpoint,
		zdr: os.Getenv("TXCO_LIVE_DECIDE_ZDR") == "1"}
	req := request(t, allTypes)
	resp, err := b.Decide(context.Background(), req, bagWithKey(key))
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := decide.Normalize(req, &resp); err != nil {
		t.Fatalf("Normalize (the live wire no longer matches the documented shape?): %v", err)
	}
	for _, a := range resp.Answers {
		t.Logf("%s: type=%s choice=%q probability=%v score=%v confidence=%v probabilities=%v", a.Key, a.Type, a.Choice,
			deref(a.Probability), deref(a.Score), deref(a.Confidence), a.Probabilities)
	}
	t.Logf("model=%s resolved=%s tokens=%d/%d cost=%v generation=%s latency=%dms",
		resp.Model, resp.ResolvedModel, resp.InputTokens, resp.OutputTokens, deref(resp.Cost), resp.GenerationID, resp.LatencyMS)
	if got := resp.Answers[1].Choice; got != "billing" {
		t.Errorf("route = %q, want billing for a double charge", got)
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
