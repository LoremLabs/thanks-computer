package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// A fake parent: records what the node sent and answers as told.
type parent struct {
	mu     sync.Mutex
	path   string
	hdr    http.Header
	body   string
	status int
	answer string
}

func newParent(t *testing.T) (*parent, *httptest.Server) {
	t.Helper()
	p := &parent{status: http.StatusOK, answer: `{"ok":true,"output":{"seq":7}}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.path, p.hdr, p.body = r.URL.Path, r.Header.Clone(), string(b)
		status, answer := p.status, p.answer
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "rid-parent-1")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

func capOp(name, meta string) operation.Operation {
	return operation.Operation{
		Stack: "pony-agent", Scope: 630, Name: "note",
		Resonator: &resonator.Resonator{Exec: "cap://" + name},
		Meta:      meta,
		Input:     `{"_txc":{"src":"http"}}`,
	}
}

func TestExecCapCallsUp(t *testing.T) {
	p, srv := newParent(t)
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL + "/", OpPayloadMax: 1 << 20}}
	ctx := WithRunGrant(context.WithValue(context.Background(), config.CtxKeyRid, "rid-node-1"), "rg1.the.token")

	payload, err := pu.ExecCap(ctx, capOp("card.note", `{"input":{"text":"hi"},"into":"_noted"}`))
	if err != nil {
		t.Fatalf("ExecCap: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_noted.seq").Int(); got != 7 {
		t.Errorf("output not at into: %s", payload.Raw)
	}
	for path, want := range map[string]string{"cap.name": "card.note", "cap.status": "200", "cap.rid": "rid-parent-1"} {
		if got := gjson.Get(payload.Raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if gjson.Get(payload.Raw, "cap.error").Exists() {
		t.Errorf("an allowed call carries cap.error: %s", payload.Raw)
	}
	if p.path != "/v1/cap/card.note" || p.hdr.Get(RunGrantHeader) != "rg1.the.token" ||
		p.hdr.Get(callerRIDHeader) != "rid-node-1" || p.hdr.Get("Content-Type") != "application/json" {
		t.Errorf("request: %s %v", p.path, p.hdr)
	}
	if p.body != `{"input":{"text":"hi"}}` {
		t.Errorf("body = %s", p.body)
	}
	if strings.Contains(payload.Raw, "rg1.the.token") {
		t.Errorf("the token reached the payload")
	}

	// No input, no into: an empty object goes up and the answer lands at _cap.
	payload, _ = pu.ExecCap(ctx, capOp("run.finish", `{}`))
	if p.body != `{"input":{}}` || gjson.Get(payload.Raw, "_cap.seq").Int() != 7 {
		t.Errorf("defaults: body=%s payload=%s", p.body, payload.Raw)
	}
}

func TestExecCapFailuresAreData(t *testing.T) {
	p, srv := newParent(t)
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL, OpPayloadMax: 1 << 20}}
	withGrant := WithRunGrant(context.Background(), "rg1.tok")

	check := func(what string, payload event.Payload, code string, status int64) {
		t.Helper()
		if got := gjson.Get(payload.Raw, "cap.error.code").String(); got != code {
			t.Errorf("%s: code = %q, want %q (%s)", what, got, code, payload.Raw)
		}
		if status > 0 {
			if got := gjson.Get(payload.Raw, "cap.error.status").Int(); got != status {
				t.Errorf("%s: status = %d, want %d", what, got, status)
			}
		}
		if !gjson.Get(payload.Raw, "_cap").Exists() {
			t.Errorf("%s: into is not set on failure: %s", what, payload.Raw)
		}
	}

	// No grant in this run.
	payload, err := pu.ExecCap(context.Background(), capOp("card.note", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	check("no grant", payload, "txco_cap_denied", 0)
	if p.path != "" {
		t.Errorf("a run with no grant called the parent")
	}

	// No parent configured.
	none := &Unit{Logger: zap.NewNop(), Conf: config.Config{}}
	payload, _ = none.ExecCap(withGrant, capOp("card.note", `{}`))
	check("no parent", payload, "txco_cap_unconfigured", 0)

	// The parent's answers.
	for _, tc := range []struct {
		status int
		answer string
		code   string
	}{
		{http.StatusUnauthorized, `{"ok":false,"error":{"code":"unauthorized"}}`, "txco_cap_denied"},
		{http.StatusForbidden, `{"ok":false,"error":{"code":"denied"}}`, "txco_cap_denied"},
		{http.StatusConflict, `{"ok":false,"error":{"code":"held"}}`, "txco_cap_held"},
		{http.StatusNotFound, `{"ok":false,"error":{"code":"no_capability"}}`, "txco_cap_unknown"},
		{http.StatusUnprocessableEntity, `{"ok":false,"error":{"code":"no_such_card","message":"c9"}}`, "no_such_card"},
		{http.StatusServiceUnavailable, `{"ok":false,"error":{"code":"unavailable"}}`, "txco_cap_unavailable"},
		{http.StatusBadGateway, `not json`, "txco_cap_http_502"},
		{http.StatusOK, `{"ok":false}`, "txco_cap_http_200"},
	} {
		p.mu.Lock()
		p.status, p.answer = tc.status, tc.answer
		p.mu.Unlock()
		payload, err = pu.ExecCap(withGrant, capOp("card.note", `{}`))
		if err != nil {
			t.Fatal(err)
		}
		check(tc.answer, payload, tc.code, int64(tc.status))
	}
	if got := gjson.Get(payload.Raw, "cap.rid").String(); got != "rid-parent-1" {
		t.Errorf("the parent's rid is not carried on failure: %s", payload.Raw)
	}

	// A malformed name is the one authoring error.
	if _, err := pu.ExecCap(withGrant, capOp("Card.Note", `{}`)); err == nil {
		t.Errorf("a malformed name dispatched")
	}
	// The parent is gone: a transport failure, not a Go error.
	srv.Close()
	payload, err = pu.ExecCap(withGrant, capOp("card.note", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	check("parent gone", payload, "txco_cap_transport", 0)
}

func TestRunGrantContext(t *testing.T) {
	if got := RunGrantFrom(context.Background()); got != "" {
		t.Errorf("empty context holds %q", got)
	}
	if got := RunGrantFrom(WithRunGrant(context.Background(), "  ")); got != "" {
		t.Errorf("blank token stored: %q", got)
	}
	if got := RunGrantFrom(WithRunGrant(context.Background(), "rg1.x")); got != "rg1.x" {
		t.Errorf("token = %q", got)
	}
	if string(capInput(`{"input":{"a":1}}`)) != `{"a":1}` || string(capInput(`{}`)) != `{}` || string(capInput(`{"input":null}`)) != `{}` {
		t.Errorf("capInput")
	}
}

// `WITH name` picks the capability from data.
func TestExecCapNameOverride(t *testing.T) {
	p, srv := newParent(t)
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL, OpPayloadMax: 1 << 20}}
	ctx := WithRunGrant(context.Background(), "rg1.tok")
	if _, err := pu.ExecCap(ctx, capOp("prairie.call", `{"name":"card.note","input":{"text":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	if p.path != "/v1/cap/card.note" {
		t.Errorf("path = %s, want the WITH name", p.path)
	}
	payload, err := pu.ExecCap(ctx, capOp("prairie.call", `{"name":"Not A Name"}`))
	if err != nil || gjson.Get(payload.Raw, "cap.error.code").String() != "txco_cap_unknown" {
		t.Errorf("bad WITH name: %v %s", err, payload.Raw)
	}
}

// The parent answers an allowed call with its headers first (200) and the
// outcome in the body: leading whitespace from the keepalive, and a failure
// carrying the status it would have had. The node reads that status.
func TestExecCapInBandStatus(t *testing.T) {
	p, srv := newParent(t)
	defer srv.Close()
	pu := &Unit{Logger: zap.NewNop(), Conf: config.Config{ParentURL: srv.URL, OpPayloadMax: 1 << 20}}
	ctx := WithRunGrant(context.Background(), "rg1.tok")
	run := func(name, meta string) string {
		t.Helper()
		payload, err := pu.ExecCap(ctx, capOp(name, meta))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return payload.Raw
	}

	p.status = http.StatusOK
	p.answer = "   \n  " + `{"ok":false,"error":{"code":"timeout","status":504}}`
	if out := run("ai.chat", `{"into":"think"}`); gjson.Get(out, "cap.error.code").String() != "txco_cap_timeout" || gjson.Get(out, "cap.error.status").Int() != 504 {
		t.Errorf("in-band timeout: %s", out)
	}
	p.answer = "  " + `{"ok":false,"error":{"code":"no_such_card","message":"c9","status":422}}`
	if out := run("card.note", `{"into":"noted"}`); gjson.Get(out, "cap.error.code").String() != "no_such_card" || gjson.Get(out, "cap.error.status").Int() != 422 {
		t.Errorf("in-band stack error: %s", out)
	}
	p.answer = "  " + `{"ok":false,"error":{"code":"no_capability","status":404}}`
	if out := run("card.note", `{"into":"noted"}`); gjson.Get(out, "cap.error.code").String() != "txco_cap_unknown" {
		t.Errorf("in-band unknown: %s", out)
	}
	p.answer = " \n" + `{"ok":true,"output":{"seq":7}}`
	if out := run("card.note", `{"into":"noted"}`); gjson.Get(out, "noted.seq").Int() != 7 || gjson.Get(out, "cap.status").Int() != 200 {
		t.Errorf("in-band ok: %s", out)
	}
}
