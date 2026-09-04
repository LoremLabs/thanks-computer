package server

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	websocketp "github.com/loremlabs/thanks-computer/chassis/server/personality/websocket"
)

type wsSent struct {
	tenant, id string
	typ        websocketp.MessageType
	data       []byte
}

type wsClosed struct {
	tenant, id string
	code       int
	reason     string
}

// stubRegistry records what the ops asked for.
type stubRegistry struct {
	enabled bool
	accepts map[string]websocketp.Accept
	sent    []wsSent
	closed  []wsClosed
	sendErr error
}

func newStubRegistry() *stubRegistry {
	return &stubRegistry{enabled: true, accepts: map[string]websocketp.Accept{}}
}

func (s *stubRegistry) Enabled() bool { return s.enabled }
func (s *stubRegistry) RecordAccept(sid string, a websocketp.Accept) error {
	s.accepts[sid] = a
	return nil
}
func (s *stubRegistry) Send(_ context.Context, tenant, id string, typ websocketp.MessageType, data []byte) error {
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, wsSent{tenant, id, typ, data})
	return nil
}
func (s *stubRegistry) CloseSession(_ context.Context, tenant, id string, code int, reason string) error {
	s.closed = append(s.closed, wsClosed{tenant, id, code, reason})
	return nil
}

func wsCtx(tenant, src, meta string) context.Context {
	ctx := processor.WithTenant(context.Background(), tenant)
	ctx = processor.WithSource(ctx, src)
	if meta != "" {
		ctx = operation.WithMeta(ctx, meta)
	}
	return ctx
}

const wsUpgradeIn = `{"_txc":{"src":"http","tenant":"acme","stack":"counter","ingress":"host:c.local","hostname_verified":true,` +
	`"websocket":{"upgrade":true,"session":{"id":"ws_01A"}}}}`

func wsErrCode(t *testing.T, raw string) string {
	t.Helper()
	return gjson.Get(raw, "_websocket.error.code").String()
}

func TestWebsocketAcceptRecordsPinnedIdentity(t *testing.T) {
	reg := newStubRegistry()
	d := websocketDeps{reg: reg}
	// The envelope claims tenant "victim"; the pinned ctx says acme. The
	// stub must see acme.
	in := strings.Replace(wsUpgradeIn, `"tenant":"acme"`, `"tenant":"victim"`, 1)
	meta := `{"state":{"email":"a@b.c","missing":null},"events":["close"],"origins":["app.example","*"],` +
		`"subprotocols":["pony.v1"],"subprotocol_required":true,"idle_timeout":"20m","max_message_bytes":1024}`
	res, err := websocketAccept(wsCtx("acme", "http", meta), d, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("error %s: %s", code, res.Raw)
	}
	if !gjson.Get(res.Raw, "_websocket.accepted").Bool() || gjson.Get(res.Raw, "_websocket.session.id").String() != "ws_01A" {
		t.Fatalf("output = %s", res.Raw)
	}
	a, ok := reg.accepts["ws_01A"]
	if !ok {
		t.Fatal("accept not recorded")
	}
	if a.Tenant != "acme" || a.Stack != "counter" || a.Ingress != "host:c.local" || !a.HostnameVerified {
		t.Errorf("identity = %+v", a)
	}
	if a.State != `{"email":"a@b.c"}` {
		t.Errorf("state = %q (nulls must be stripped)", a.State)
	}
	if !a.Events[websocketp.EventClose] {
		t.Error("close event not recorded")
	}
	if !a.AnyOrigin || len(a.Origins) != 1 || a.Origins[0] != "app.example" {
		t.Errorf("origins = %v any=%v", a.Origins, a.AnyOrigin)
	}
	if len(a.Subprotocols) != 1 || !a.SubprotocolRequired {
		t.Errorf("subprotocols = %v required=%v", a.Subprotocols, a.SubprotocolRequired)
	}
	if a.IdleTimeout.Minutes() != 20 || a.MaxMessageBytes != 1024 {
		t.Errorf("overrides = %v %d", a.IdleTimeout, a.MaxMessageBytes)
	}
}

func TestWebsocketAcceptRejections(t *testing.T) {
	reg := newStubRegistry()
	d := websocketDeps{reg: reg}
	cases := []struct {
		name string
		ctx  context.Context
		in   string
		code string
	}{
		{"no tenant", operation.WithMeta(context.Background(), "{}"), wsUpgradeIn, "txco_websocket_no_tenant"},
		{"not an upgrade run", wsCtx("acme", "http", "{}"), `{"_txc":{"stack":"counter"}}`, "txco_websocket_not_upgrade"},
		{"websocket-sourced run", wsCtx("acme", "websocket", "{}"), wsUpgradeIn, "txco_websocket_not_upgrade"},
		{"state not an object", wsCtx("acme", "http", `{"state":"x"}`), wsUpgradeIn, "txco_websocket_bad_argument"},
		{"state too large", wsCtx("acme", "http", `{"state":{"blob":"`+strings.Repeat("x", websocketp.StateMaxBytes)+`"}}`), wsUpgradeIn, "txco_websocket_state_too_large"},
		{"unknown event", wsCtx("acme", "http", `{"events":["open"]}`), wsUpgradeIn, "txco_websocket_bad_argument"},
		{"empty origin entry", wsCtx("acme", "http", `{"origins":[""]}`), wsUpgradeIn, "txco_websocket_bad_argument"},
		{"required without list", wsCtx("acme", "http", `{"subprotocol_required":true}`), wsUpgradeIn, "txco_websocket_bad_argument"},
		{"bad idle_timeout", wsCtx("acme", "http", `{"idle_timeout":"soon"}`), wsUpgradeIn, "txco_websocket_bad_argument"},
		{"bad max_message_bytes", wsCtx("acme", "http", `{"max_message_bytes":-1}`), wsUpgradeIn, "txco_websocket_bad_argument"},
	}
	for _, tc := range cases {
		res, _ := websocketAccept(tc.ctx, d, []byte(tc.in))
		if got := wsErrCode(t, res.Raw); got != tc.code {
			t.Errorf("%s: code = %q, want %q (%s)", tc.name, got, tc.code, res.Raw)
		}
	}
	if len(reg.accepts) != 0 {
		t.Errorf("rejections recorded accepts: %v", reg.accepts)
	}
	// Null options (a WITH path that resolved to nothing) are simply absent.
	res, _ := websocketAccept(wsCtx("acme", "http", `{"state":null,"events":null,"origins":null,"idle_timeout":null}`), d, []byte(wsUpgradeIn))
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("null options: %s", res.Raw)
	}
	if a := reg.accepts["ws_01A"]; a.State != "" || len(a.Events) != 0 || a.IdleTimeout != 0 {
		t.Errorf("null options changed the accept: %+v", a)
	}

	reg.enabled = false
	res, _ = websocketAccept(wsCtx("acme", "http", "{}"), d, []byte(wsUpgradeIn))
	if got := wsErrCode(t, res.Raw); got != "txco_websocket_disabled" {
		t.Errorf("disabled: %q", got)
	}
	res, _ = websocketAccept(wsCtx("acme", "http", "{}"), websocketDeps{}, []byte(wsUpgradeIn))
	if got := wsErrCode(t, res.Raw); got != "txco_websocket_disabled" {
		t.Errorf("nil registry: %q", got)
	}
}

func TestWebsocketSendAndReply(t *testing.T) {
	reg := newStubRegistry()
	d := websocketDeps{reg: reg}
	sessIn := `{"_txc":{"src":"websocket","websocket":{"session":{"id":"ws_S"}}}}`

	// text as a string
	res, _ := websocketSend(wsCtx("acme", "http", `{"session_id":"ws_S","text":"hello"}`), d, []byte(`{}`))
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("send: %s", res.Raw)
	}
	if got := reg.sent[len(reg.sent)-1]; got.tenant != "acme" || got.id != "ws_S" || string(got.data) != "hello" || got.typ != websocketp.MessageText {
		t.Errorf("sent = %+v", got)
	}
	if gjson.Get(res.Raw, "_websocket.sent.bytes").Int() != 5 || gjson.Get(res.Raw, "_websocket.sent.type").String() != "text" {
		t.Errorf("output = %s", res.Raw)
	}
	// text as an object → JSON text (WITH text.count = ._kv)
	res, _ = websocketSend(wsCtx("acme", "http", `{"session_id":"ws_S","text":{"count":3}}`), d, []byte(`{}`))
	if got := string(reg.sent[len(reg.sent)-1].data); got != `{"count":3}` {
		t.Errorf("object text = %q", got)
	}
	// data → binary
	b64 := base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	res, _ = websocketSend(wsCtx("acme", "http", `{"session_id":"ws_S","data":"`+b64+`"}`), d, []byte(`{}`))
	if got := reg.sent[len(reg.sent)-1]; got.typ != websocketp.MessageBinary || len(got.data) != 3 {
		t.Errorf("binary = %+v", got)
	}
	// reply: implicit session from the envelope, gated on the pinned source
	res, _ = websocketReply(wsCtx("acme", "websocket", `{"text":"r"}`), d, []byte(sessIn))
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("reply: %s", res.Raw)
	}
	if got := reg.sent[len(reg.sent)-1]; got.id != "ws_S" || string(got.data) != "r" {
		t.Errorf("reply sent = %+v", got)
	}
	res, _ = websocketReply(wsCtx("acme", "http", `{"text":"r"}`), d, []byte(sessIn))
	if got := wsErrCode(t, res.Raw); got != "txco_websocket_not_session_run" {
		t.Errorf("reply from http run: %q", got)
	}

	// argument errors
	for name, meta := range map[string]string{
		"no session":  `{"text":"x"}`,
		"null text":   `{"session_id":"ws_S","text":null}`,
		"both":        `{"session_id":"ws_S","text":"x","data":"AA=="}`,
		"bad base64":  `{"session_id":"ws_S","data":"@@"}`,
		"data number": `{"session_id":"ws_S","data":5}`,
	} {
		res, _ := websocketSend(wsCtx("acme", "http", meta), d, []byte(`{}`))
		if got := wsErrCode(t, res.Raw); got != "txco_websocket_bad_argument" {
			t.Errorf("%s: %q", name, got)
		}
	}
	// registry errors map to codes
	for err, code := range map[error]string{
		websocketp.ErrSessionNotFound: "txco_websocket_session_not_found",
		websocketp.ErrSessionClosed:   "txco_websocket_session_closed",
		websocketp.ErrWriteTimeout:    "txco_websocket_write_timeout",
		websocketp.ErrMessageTooLarge: "txco_websocket_message_too_large",
		errors.New("boom"):            "txco_websocket_error",
	} {
		reg.sendErr = err
		res, _ := websocketSend(wsCtx("acme", "http", `{"session_id":"ws_S","text":"x"}`), d, []byte(`{}`))
		if got := wsErrCode(t, res.Raw); got != code {
			t.Errorf("%v: %q, want %q", err, got, code)
		}
	}
	reg.sendErr = nil
	// custom into
	res, _ = websocketSend(wsCtx("acme", "http", `{"session_id":"ws_S","text":"x","into":"_out"}`), d, []byte(`{}`))
	if gjson.Get(res.Raw, "_out.sent.session_id").String() != "ws_S" {
		t.Errorf("into: %s", res.Raw)
	}
}

func TestWebsocketClose(t *testing.T) {
	reg := newStubRegistry()
	d := websocketDeps{reg: reg}
	sessIn := `{"_txc":{"src":"websocket","websocket":{"session":{"id":"ws_S"}}}}`

	res, _ := websocketClose(wsCtx("acme", "http", `{"session_id":"ws_S","code":4001,"reason":"done"}`), d, []byte(`{}`))
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("close: %s", res.Raw)
	}
	if got := reg.closed[0]; got.tenant != "acme" || got.id != "ws_S" || got.code != 4001 || got.reason != "done" {
		t.Errorf("closed = %+v", got)
	}
	// implicit session + default code inside a session run
	res, _ = websocketClose(wsCtx("acme", "websocket", `{}`), d, []byte(sessIn))
	if code := wsErrCode(t, res.Raw); code != "" {
		t.Fatalf("implicit close: %s", res.Raw)
	}
	if got := reg.closed[1]; got.id != "ws_S" || got.code != 1000 {
		t.Errorf("implicit closed = %+v", got)
	}
	// not implicit outside a session run
	res, _ = websocketClose(wsCtx("acme", "http", `{}`), d, []byte(sessIn))
	if got := wsErrCode(t, res.Raw); got != "txco_websocket_bad_argument" {
		t.Errorf("implicit from http: %q", got)
	}
	for name, meta := range map[string]string{
		"reserved code": `{"session_id":"ws_S","code":1005}`,
		"string code":   `{"session_id":"ws_S","code":"1000"}`,
		"low code":      `{"session_id":"ws_S","code":999}`,
	} {
		res, _ := websocketClose(wsCtx("acme", "http", meta), d, []byte(`{}`))
		if got := wsErrCode(t, res.Raw); got != "txco_websocket_invalid_close_code" {
			t.Errorf("%s: %q", name, got)
		}
	}
	res, _ = websocketClose(wsCtx("acme", "http", `{"session_id":"ws_S","reason":"`+strings.Repeat("r", 124)+`"}`), d, []byte(`{}`))
	if got := wsErrCode(t, res.Raw); got != "txco_websocket_bad_argument" {
		t.Errorf("long reason: %q", got)
	}
}
