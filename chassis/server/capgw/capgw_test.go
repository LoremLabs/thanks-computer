package capgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/server/grantgw"
)

type fake struct {
	g       *Gateway
	verdict grantgw.Verdict
	calls   []grantgw.Call
	seen    []string
	answer  func(envelope string) (string, error)
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{}
	f.g = &Gateway{ctx: context.Background(), log: zap.NewNop(), maxWait: time.Second, maxBody: 1 << 20}
	f.g.invoke = func(_ context.Context, c grantgw.Call) grantgw.Verdict {
		f.calls = append(f.calls, c)
		return f.verdict
	}
	f.g.run = func(_ context.Context, payload string) (string, error) {
		f.seen = append(f.seen, payload)
		if f.answer == nil {
			return payload, nil
		}
		return f.answer(payload)
	}
	return f
}

func allowed() grantgw.Verdict {
	who, _ := authn.ParsePrincipal("service:pony-research")
	return grantgw.Verdict{Allowed: true, Tenant: "acme", Grant: authn.RunGrant{
		ID: "rgr_1", Run: "t1/run-9", Generation: 2, Principal: who, Stack: "loop", Workspace: "pony/research", TraceID: "tr_1",
	}}
}

func (f *fake) post(t *testing.T, name, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := mux.NewRouter()
	r.Path("/v1/cap/{name}").HandlerFunc(f.g.Handle).Methods(http.MethodPost)
	req := httptest.NewRequest(http.MethodPost, "/v1/cap/"+name, strings.NewReader(body))
	if token != "" {
		req.Header.Set(Header, token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func code(w *httptest.ResponseRecorder) string {
	return gjson.Get(w.Body.String(), "error.code").String()
}

func TestHandleAnswers(t *testing.T) {
	f := newFake(t)
	f.verdict = allowed()
	f.answer = func(env string) (string, error) {
		out, _ := sjson.SetRaw(env, "_cap.output", `{"seq":4,"noted":true}`)
		return out, nil
	}
	w := f.post(t, "card.note", "rg1.tok", `{"input":{"text":"hi"}}`, CallerRIDHeader, "rid-node-1")
	if w.Code != http.StatusOK || gjson.Get(w.Body.String(), "ok").Bool() != true || gjson.Get(w.Body.String(), "output.seq").Int() != 4 {
		t.Fatalf("answer: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("X-Request-ID") == "" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("headers: %v", w.Header())
	}
	if len(f.calls) != 1 || f.calls[0].Token != "rg1.tok" || f.calls[0].Name != "card.note" || string(f.calls[0].Input) != `{"text":"hi"}` {
		t.Errorf("invoke saw %+v", f.calls)
	}
	env := f.seen[0]
	for path, want := range map[string]string{
		"_txc.src": "cap", "_txc.cap.tenant": "acme", "_txc.cap.name": "card.note", "_txc.cap.run": "t1/run-9",
		"_txc.cap.grant": "rgr_1", "_txc.cap.generation": "2", "_txc.cap.principal.id": "service:pony-research",
		"_txc.cap.principal.kind": "service", "_txc.cap.stack": "loop", "_txc.cap.workspace": "pony/research",
		"_txc.cap.trace": "tr_1", "_txc.cap.caller.rid": "rid-node-1", "_txc.cap.input.text": "hi",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if rid := gjson.Get(env, "_txc.rid").String(); rid == "" || rid != w.Header().Get("X-Request-ID") {
		t.Errorf("rid %q vs header %q", rid, w.Header().Get("X-Request-ID"))
	}
	if strings.Contains(env, "rg1.tok") {
		t.Errorf("the token reached the _cap envelope")
	}
}

func TestHandleRefusals(t *testing.T) {
	f := newFake(t)

	if w := f.post(t, "card.note", "", `{}`); w.Code != http.StatusUnauthorized || code(w) != "unauthorized" {
		t.Errorf("no token: %d %s", w.Code, w.Body.String())
	}
	if w := f.post(t, "Card.Note", "rg1.tok", `{}`); w.Code != http.StatusNotFound || code(w) != "no_capability" {
		t.Errorf("bad name: %d %s", w.Code, w.Body.String())
	}
	if w := f.post(t, "card.note", "rg1.tok", `[1,2]`); w.Code != http.StatusBadRequest {
		t.Errorf("not an object: %d %s", w.Code, w.Body.String())
	}
	if len(f.calls) != 0 {
		t.Fatalf("invoked before the request was whole: %+v", f.calls)
	}

	f.verdict = grantgw.Verdict{Reason: "token"} // not identified
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusUnauthorized || code(w) != "unauthorized" {
		t.Errorf("bad token: %d %s", w.Code, w.Body.String())
	}
	f.verdict = allowed()
	f.verdict.Allowed, f.verdict.Reason = false, "allowlist"
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusForbidden || code(w) != "denied" || strings.Contains(w.Body.String(), "allowlist") {
		t.Errorf("refused: %d %s (the reason must not reach the caller)", w.Code, w.Body.String())
	}
	f.verdict = allowed()
	f.verdict.Allowed, f.verdict.Held, f.verdict.Reason = false, true, "held"
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusConflict || code(w) != "held" {
		t.Errorf("held: %d %s", w.Code, w.Body.String())
	}
	f.verdict = grantgw.Verdict{Unavailable: true, Reason: "identity store"}
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusServiceUnavailable || code(w) != "unavailable" {
		t.Errorf("unavailable: %d %s", w.Code, w.Body.String())
	}
	if len(f.seen) != 0 {
		t.Errorf("the _cap stack ran %d times on refusals", len(f.seen))
	}
}

func TestHandleRunOutcomes(t *testing.T) {
	f := newFake(t)
	f.verdict = allowed()

	// No rule answered: the headers were 200 (sent before the run), the
	// body says what happened and the status it would have carried.
	if w := f.post(t, "card.note", "rg1.tok", ``); w.Code != http.StatusOK || code(w) != "no_capability" || status(w) != http.StatusNotFound {
		t.Errorf("no answer: %d %s", w.Code, w.Body.String())
	}
	// An empty body is an empty input.
	if got := gjson.Get(f.seen[0], "_txc.cap.input").Raw; got != `{}` {
		t.Errorf("input = %s, want {}", got)
	}
	// The stack's own error.
	f.answer = func(env string) (string, error) {
		out, _ := sjson.Set(env, "_cap.error.code", "no_such_card")
		out, _ = sjson.Set(out, "_cap.error.message", "card c9 is not on this board")
		return out, nil
	}
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusOK || code(w) != "no_such_card" || status(w) != http.StatusUnprocessableEntity ||
		gjson.Get(w.Body.String(), "error.message").String() != "card c9 is not on this board" {
		t.Errorf("stack error: %d %s", w.Code, w.Body.String())
	}
	// The run failed.
	f.answer = func(string) (string, error) { return "", errors.New("pipeline error: boom") }
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusOK || code(w) != "unavailable" || status(w) != http.StatusServiceUnavailable {
		t.Errorf("run failed: %d %s", w.Code, w.Body.String())
	}
	// Admission refused the tenant.
	f.answer = func(env string) (string, error) {
		out, _ := sjson.Set(env, "_txc.admission.denied", true)
		return out, nil
	}
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusOK || status(w) != http.StatusServiceUnavailable {
		t.Errorf("admission: %d %s", w.Code, w.Body.String())
	}
	// The run took too long.
	f.g.maxWait = 20 * time.Millisecond
	f.answer = func(string) (string, error) { time.Sleep(60 * time.Millisecond); return "", context.DeadlineExceeded }
	if w := f.post(t, "card.note", "rg1.tok", `{}`); w.Code != http.StatusOK || code(w) != "timeout" || status(w) != http.StatusGatewayTimeout {
		t.Errorf("timeout: %d %s", w.Code, w.Body.String())
	}
	var a answer
	if err := json.Unmarshal([]byte(`{"ok":false,"error":{"code":"x","status":422}}`), &a); err != nil || a.OK || a.Error.Code != "x" || a.Error.Status != 422 {
		t.Errorf("answer shape: %+v %v", a, err)
	}
}

// status reads the status a 200 body carries for its failure.
func status(w *httptest.ResponseRecorder) int {
	return int(gjson.Get(w.Body.String(), "error.status").Int())
}

// The headers of an allowed call reach the client before the run ends, and
// the connection is fed while it runs: a proxy with a header timeout and an
// idle limit sees both satisfied. Over a real listener, since a recorder
// cannot say when headers were flushed.
func TestHandleSendsHeadersEarly(t *testing.T) {
	f := newFake(t)
	f.verdict = allowed()
	f.g.maxWait = 5 * time.Second
	started := make(chan struct{})
	release := make(chan struct{})
	f.answer = func(env string) (string, error) {
		close(started)
		<-release
		out, _ := sjson.SetRaw(env, "_cap.output", `{"seq":7}`)
		return out, nil
	}
	r := mux.NewRouter()
	r.HandleFunc("/v1/cap/{name}", f.g.Handle).Methods(http.MethodPost)
	srv := httptest.NewServer(r)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/cap/card.note", strings.NewReader(`{"input":{"text":"hi"}}`))
	req.Header.Set(Header, "rg1.tok")
	resp, err := http.DefaultClient.Do(req) // returns once the headers are in
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	<-started
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// The body is not there yet: the run has not ended.
	select {
	case <-release:
		t.Fatal("released early")
	default:
	}
	close(release)
	body, _ := io.ReadAll(resp.Body)
	if !gjson.ValidBytes(body) || !gjson.GetBytes(body, "ok").Bool() || gjson.GetBytes(body, "output.seq").Int() != 7 {
		t.Fatalf("body: %s", body)
	}
}

// The feeding: a space a tick, until the body; then nothing more.
func TestEarlyFeedsThenFinishes(t *testing.T) {
	w := httptest.NewRecorder()
	e := startEarly(w)
	e.mu.Lock()
	_, _ = io.WriteString(e.w, " ") // what a tick does
	e.mu.Unlock()
	e.fail(http.StatusGatewayTimeout, "timeout", "")
	e.fail(http.StatusNotFound, "no_capability", "again") // a second finish is ignored
	body := w.Body.String()
	if !strings.HasPrefix(body, " ") || !gjson.Valid(body) || gjson.Get(body, "error.code").String() != "timeout" ||
		gjson.Get(body, "error.status").Int() != 504 || strings.Count(body, "error") != 1 {
		t.Fatalf("body: %q", body)
	}
}
