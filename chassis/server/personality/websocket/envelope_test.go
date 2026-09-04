package websocket

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func baseInput() envelopeInput {
	return envelopeInput{
		rid:              "rid1",
		now:              time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		tenant:           "acme",
		stack:            "counter",
		ingress:          "host:counter.local.thanks.computer",
		hostnameVerified: true,
		sessionID:        "ws_01TEST",
		subprotocol:      "pony.v1",
		connectedAt:      time.Date(2026, 9, 4, 11, 59, 0, 0, time.UTC),
		seq:              3,
		state:            `{"email":"a@b.c"}`,
		host:             "counter.local.thanks.computer",
		path:             "/ws",
		origin:           "https://counter.local.thanks.computer",
		userAgent:        "test/1",
		clientIP:         "203.0.113.7",
	}
}

func TestBuildEnvelopeMessageText(t *testing.T) {
	in := baseInput()
	in.phase = phaseMessage
	in.msgType = MessageText
	in.msg = []byte(`{"type":"increment"}`)
	raw := buildEnvelope(in)
	if !gjson.Valid(raw) {
		t.Fatalf("invalid JSON: %s", raw)
	}
	want := map[string]string{
		"_txc.src":                            "websocket",
		"_txc.rid":                            "rid1",
		"_ts":                                 "2026-09-04T12:00:00Z",
		"_txc.client.ip":                      "203.0.113.7",
		"_txc.route.tenant":                   "acme",
		"_txc.route.stack":                    "counter/_websocket",
		"_txc.route.to":                       "counter/_websocket/0",
		"_txc.route.ingress":                  "host:counter.local.thanks.computer",
		"_txc.websocket.tenant":               "acme",
		"_txc.websocket.phase":                "message",
		"_txc.websocket.session.id":           "ws_01TEST",
		"_txc.websocket.session.stack":        "counter",
		"_txc.websocket.session.subprotocol":  "pony.v1",
		"_txc.websocket.session.connected_at": "2026-09-04T11:59:00Z",
		"_txc.websocket.session.state.email":  "a@b.c",
		"_txc.websocket.req.host":             "counter.local.thanks.computer",
		"_txc.websocket.req.path":             "/ws",
		"_txc.websocket.req.origin":           "https://counter.local.thanks.computer",
		"_txc.websocket.req.user_agent":       "test/1",
		"_txc.websocket.msg.type":             "text",
		"_txc.websocket.msg.text":             `{"type":"increment"}`,
	}
	for path, exp := range want {
		if got := gjson.Get(raw, path).String(); got != exp {
			t.Errorf("%s = %q, want %q", path, got, exp)
		}
	}
	if got := gjson.Get(raw, "_txc.route.hostname_verified").Bool(); !got {
		t.Error("hostname_verified not stamped true")
	}
	if got := gjson.Get(raw, "_txc.websocket.session.seq").Int(); got != 3 {
		t.Errorf("seq = %d, want 3", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.msg.bytes").Int(); got != 20 {
		t.Errorf("bytes = %d, want 20", got)
	}
	if gjson.Get(raw, "_txc.websocket.msg.data").Exists() {
		t.Error("text message must not carry msg.data")
	}
	if gjson.Get(raw, "_txc.flag_private").Exists() {
		t.Error("flag_private stamped without DebugPrivate")
	}
}

func TestBuildEnvelopeBinaryAndClose(t *testing.T) {
	in := baseInput()
	in.phase = phaseMessage
	in.msgType = MessageBinary
	in.msg = []byte{0, 1, 2, 255}
	in.private = true
	in.state = ""
	raw := buildEnvelope(in)
	if got := gjson.Get(raw, "_txc.websocket.msg.type").String(); got != "binary" {
		t.Errorf("type = %q", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.msg.data").String(); got != base64.StdEncoding.EncodeToString(in.msg) {
		t.Errorf("data = %q", got)
	}
	if gjson.Get(raw, "_txc.websocket.msg.text").Exists() {
		t.Error("binary message must not carry msg.text")
	}
	if gjson.Get(raw, "_txc.websocket.session.state").Exists() {
		t.Error("empty state must be absent, not null")
	}
	if !gjson.Get(raw, "_txc.flag_private").Bool() {
		t.Error("flag_private missing with DebugPrivate")
	}

	in = baseInput()
	in.phase = phaseClose
	in.closeCode, in.closeReason, in.closeInitiator = 1001, "going away", "chassis"
	raw = buildEnvelope(in)
	if got := gjson.Get(raw, "_txc.websocket.phase").String(); got != "close" {
		t.Errorf("phase = %q", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.close.code").Int(); got != 1001 {
		t.Errorf("close.code = %d", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.close.initiated_by").String(); got != "chassis" {
		t.Errorf("initiated_by = %q", got)
	}
	if gjson.Get(raw, "_txc.websocket.msg").Exists() {
		t.Error("close envelope must not carry msg")
	}
}

// newUpgradeRequest builds a well-formed RFC 6455 handshake request.
func newUpgradeRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	return r
}

func TestIsUpgradeAndSubprotocols(t *testing.T) {
	r := newUpgradeRequest(t, "/ws")
	if !IsUpgrade(r) {
		t.Fatal("IsUpgrade = false for a well-formed upgrade")
	}
	r.Header.Set("Connection", "keep-alive, Upgrade")
	r.Header.Set("Upgrade", "WebSocket")
	if !IsUpgrade(r) {
		t.Fatal("IsUpgrade must be token- and case-insensitive")
	}
	r.Header.Add("Sec-WebSocket-Protocol", "pony.v1, chat")
	r.Header.Add("Sec-WebSocket-Protocol", "graphql-ws")
	got := RequestedSubprotocols(r)
	if len(got) != 3 || got[0] != "pony.v1" || got[1] != "chat" || got[2] != "graphql-ws" {
		t.Fatalf("subprotocols = %v", got)
	}
	r.Method = "POST"
	if IsUpgrade(r) {
		t.Fatal("a POST is never an upgrade")
	}
}

func TestNewSessionIDShape(t *testing.T) {
	c := NewController(nil, nil)
	a, b := c.NewSessionID(), c.NewSessionID()
	if len(a) != 3+26 || a[:3] != "ws_" || a == b {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestValidCloseCode(t *testing.T) {
	for _, ok := range []int{1000, 1001, 1003, 1007, 1008, 1011, 3000, 4999} {
		if !ValidCloseCode(ok) {
			t.Errorf("%d should be valid", ok)
		}
	}
	for _, bad := range []int{999, 1002, 1004, 1005, 1006, 1012, 1015, 2999, 5000} {
		if ValidCloseCode(bad) {
			t.Errorf("%d should be invalid", bad)
		}
	}
}
