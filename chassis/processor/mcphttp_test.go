package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// mcpStub is a minimal MCP-over-HTTP server for tests. It records
// the JSON-RPC methods it received in order and lets each test
// supply a tools/call handler.
type mcpStub struct {
	URL string

	mu       sync.Mutex
	methods  []string         // JSON-RPC methods received, in order
	calls    []*http.Request  // raw requests (for header inspection)
	bodies   [][]byte         // raw request bodies (for body inspection)
	sessions []string         // Mcp-Session-Id header on each request

	// SessionID returned from initialize; empty = stateless.
	SessionID string

	// InitErr, if non-nil, makes initialize fail with a JSON-RPC error.
	InitErr *struct {
		Code    int
		Message string
	}

	// OnToolsCall, if set, produces the tools/call response body
	// (a full JSON-RPC reply). Default: empty content result.
	OnToolsCall func(req []byte) []byte

	// ToolsListRequiresSession, if true, returns 400 when
	// tools/list is called without Mcp-Session-Id.
	ToolsListRequiresSession bool

	// OnToolsList builds the tools/list response body.
	OnToolsList func() []byte

	srv *httptest.Server
}

func newMCPStub(t *testing.T) *mcpStub {
	t.Helper()
	s := &mcpStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *mcpStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	method := gjson.GetBytes(body, "method").String()
	id := gjson.GetBytes(body, "id").Int()
	hasID := gjson.GetBytes(body, "id").Exists()

	s.mu.Lock()
	s.methods = append(s.methods, method)
	s.calls = append(s.calls, r.Clone(context.Background()))
	s.bodies = append(s.bodies, body)
	s.sessions = append(s.sessions, r.Header.Get(mcpSessionHeader))
	s.mu.Unlock()

	switch method {
	case "initialize":
		if s.InitErr != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%q}}`,
				id, s.InitErr.Code, s.InitErr.Message)
			return
		}
		if s.SessionID != "" {
			w.Header().Set(mcpSessionHeader, s.SessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"serverInfo":{"name":"stub","version":"0"}}}`,
			id, mcpProtocolVersion)
	case "notifications/initialized":
		// Notifications carry no id; respond 202 with empty body.
		w.WriteHeader(http.StatusAccepted)
	case "tools/call":
		if s.OnToolsCall != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(s.OnToolsCall(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[]}}`, id)
	case "tools/list":
		if s.ToolsListRequiresSession && r.Header.Get(mcpSessionHeader) == "" {
			http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s.OnToolsList != nil {
			_, _ = w.Write(s.OnToolsList())
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, id)
	default:
		_ = hasID
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (s *mcpStub) recordedMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.methods))
	copy(out, s.methods)
	return out
}

func (s *mcpStub) recordedSessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.sessions))
	copy(out, s.sessions)
	return out
}

func mcpExecURL(stubURL, tool string) string {
	// stubURL is "http://127.0.0.1:..." — prepend mcp+ to make the
	// chassis dispatch to ExecMCPHTTP.
	return "mcp+" + stubURL + "#" + tool
}

func mcpOp(stubURL, tool, input, meta string) operation.Operation {
	return operation.Operation{
		Input:     input,
		Meta:      meta,
		Resonator: &resonator.Resonator{Exec: mcpExecURL(stubURL, tool)},
	}
}

func TestExecMCPHTTPHappyPathText(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}

	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "summarize", `{"q":"x"}`, ""))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.Get(payload.Raw, "text").String(); got != "hi" {
		t.Errorf("text projection = %q, want %q (raw=%s)", got, "hi", payload.Raw)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call"}
	if got := stub.recordedMethods(); !equalSlices(got, want) {
		t.Errorf("method sequence = %v, want %v", got, want)
	}
}

// TestExecMCPHTTPSkipsNotificationFramesBeforeResponse — regression
// for the prod bug where DeepWiki's tools/call response interleaved
// `notifications/message` log frames with the actual JSON-RPC
// response (both arrive as `event: message` SSE frames). The
// previous ExtractSSE returned the first frame (the notification),
// which had no `result`, so projectMCPResult produced `{}` and the
// envelope's `.text` was empty downstream.
func TestExecMCPHTTPSkipsNotificationFramesBeforeResponse(t *testing.T) {
	// Hand-rolled SSE stub: text/event-stream content-type + the
	// exact interleave pattern we observed in production (ping
	// comment, log notification, then the response).
	sseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		switch method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q}}`, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Interleaved: heartbeat, two log notifications, then
			// the real response. Real-world MCP servers commonly
			// emit progress/log notifications during long-running
			// tool calls.
			_, _ = w.Write([]byte(": ping - heartbeat\r\n\r\n"))
			_, _ = w.Write([]byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":{\"msg\":\"Processing query…\"}}}\r\n\r\n"))
			_, _ = w.Write([]byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"percent\":50}}\r\n\r\n"))
			_, _ = w.Write([]byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"the-answer\"}]}}\r\n\r\n"))
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(sseHandler)
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(),
		mcpOp(srv.URL, "ask", `{}`, ""))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.Get(payload.Raw, "text").String(); got != "the-answer" {
		t.Fatalf("expected `text` projection to skip notifications and reach the response; got %q (raw=%s)",
			got, payload.Raw)
	}
}

func TestExecMCPHTTPHappyPathStructured(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}`)
	}
	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, ""))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	arr := gjson.Get(payload.Raw, "content").Array()
	if len(arr) != 2 {
		t.Fatalf("content len = %d, want 2 (raw=%s)", len(arr), payload.Raw)
	}
	if arr[0].Get("text").String() != "a" || arr[1].Get("text").String() != "b" {
		t.Errorf("content blocks lost: %s", payload.Raw)
	}
}

func TestExecMCPHTTPCarriesSessionID(t *testing.T) {
	stub := newMCPStub(t)
	stub.SessionID = "abc-123"
	pu, _ := newTestUnit(t)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, "")); err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	sids := stub.recordedSessions()
	if len(sids) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(sids))
	}
	if sids[0] != "" {
		t.Errorf("initialize carried session id %q (should be empty pre-handshake)", sids[0])
	}
	for i, phase := range []string{"notifications/initialized", "tools/call"} {
		if sids[i+1] != "abc-123" {
			t.Errorf("%s did not carry Mcp-Session-Id: got %q", phase, sids[i+1])
		}
	}
}

func TestExecMCPHTTPStatelessServerWorks(t *testing.T) {
	stub := newMCPStub(t) // SessionID = "" → stateless
	pu, _ := newTestUnit(t)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, "")); err != nil {
		t.Fatalf("ExecMCPHTTP on stateless server: %v", err)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call"}
	if got := stub.recordedMethods(); !equalSlices(got, want) {
		t.Errorf("method sequence = %v, want %v", got, want)
	}
	for _, sid := range stub.recordedSessions() {
		if sid != "" {
			t.Errorf("stateless server saw Mcp-Session-Id %q (chassis should not invent one)", sid)
		}
	}
}

func TestExecMCPHTTPInitializeFailureSkipsToolsCall(t *testing.T) {
	stub := newMCPStub(t)
	stub.InitErr = &struct {
		Code    int
		Message string
	}{Code: -32600, Message: "unsupported version"}

	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, ""))
	if err == nil {
		t.Fatal("expected initialize failure to surface, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported version") {
		t.Errorf("err = %v, want one containing 'unsupported version'", err)
	}
	if !strings.Contains(payload.Meta, "mcp-http-init") {
		t.Errorf("payload meta missing phase tag: %q", payload.Meta)
	}
	// Sequence should stop after initialize.
	if got := stub.recordedMethods(); len(got) != 1 || got[0] != "initialize" {
		t.Errorf("expected only initialize, got %v", got)
	}
}

func TestExecMCPHTTPSurfacesToolCallRPCError(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad arg"}}`)
	}
	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, ""))
	if err == nil {
		t.Fatal("expected tools/call RPC error, got nil")
	}
	if !strings.Contains(err.Error(), "bad arg") {
		t.Errorf("err = %v, want 'bad arg'", err)
	}
	if !strings.Contains(payload.Meta, "mcp-http-call") {
		t.Errorf("payload meta missing call phase: %q", payload.Meta)
	}
	if payload.Type != 0 /* event.Null */ {
		// don't import event for the constant; the failure shape is
		// what matters
	}
}

func TestExecMCPHTTPRequiresToolFragment(t *testing.T) {
	pu, _ := newTestUnit(t)
	op := operation.Operation{
		Input:     `{}`,
		Resonator: &resonator.Resonator{Exec: "mcp+http://127.0.0.1:1/mcp"}, // no fragment
	}
	_, _, err := pu.Exec(context.Background(), op)
	if err == nil {
		t.Fatal("expected missing-fragment error, got nil")
	}
	if !strings.Contains(err.Error(), "missing #tool-name fragment") {
		t.Errorf("err = %v, want 'missing #tool-name fragment'", err)
	}
}

func TestExecMCPHTTPInheritsEgressGuard(t *testing.T) {
	stub := newMCPStub(t)
	addr := strings.TrimPrefix(stub.URL, "http://")
	g := addrDenyGuard{blocked: addr}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout: 2 * time.Second,
		Control: egress.DialControl(g),
	}).DialContext
	pu, _ := newTestUnit(t)
	pu.HTTPClient = &http.Client{Transport: tr, Timeout: 2 * time.Second}

	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "t", `{}`, ""))
	if err == nil {
		t.Fatal("expected guard to block initialize dial, got nil")
	}
	if !strings.Contains(payload.Meta, "mcp-http-init") {
		t.Errorf("expected init phase tag, got meta %q", payload.Meta)
	}
	// The stub should have received no requests.
	if got := stub.recordedMethods(); len(got) != 0 {
		t.Errorf("stub recorded %v despite guard block", got)
	}
}

func TestExecMCPHTTPRespectsContextCancel(t *testing.T) {
	// Stub that hangs on initialize until either the client
	// context cancels OR the test signals teardown via `hang`.
	// Without the hang signal, srv.Close would block draining the
	// in-flight connection if the server doesn't notice the
	// client-side cancel quickly enough.
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(hang)
		srv.Close()
	})

	pu, _ := newTestUnit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := pu.ExecMCPHTTP(ctx, mcpOp(srv.URL, "t", `{}`, ""))
	if err == nil {
		t.Fatal("expected ctx-cancel error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ExecMCPHTTP did not honor ctx cancel; took %v", elapsed)
	}
}

func TestExtractMCPArguments(t *testing.T) {
	tests := []struct {
		name, envelope, want string
	}{
		{
			"web body decoded",
			// _txc.web.req.body = base64("{\"q\":\"hi\"}")
			`{"_ts":"now","_txc":{"web":{"req":{"body":"eyJxIjoiaGkifQ=="}}}}`,
			`{"q":"hi"}`,
		},
		{
			"envelope minus _txc and _ts",
			`{"_ts":"now","_txc":{"rid":"x"},"repoName":"r","question":"q"}`,
			`{"repoName":"r","question":"q"}`,
		},
		{
			"empty envelope",
			``,
			`{}`,
		},
		{
			"envelope with only plumbing",
			`{"_ts":"now","_txc":{"src":"http"}}`,
			`{}`,
		},
		{
			"web body wins over envelope root",
			// even with stray keys at root, body decode takes precedence
			`{"_ts":"now","_txc":{"web":{"req":{"body":"eyJ3aW5zIjoidHJ1ZSJ9"}}},"loser":"yes"}`,
			`{"wins":"true"}`,
		},
		{
			"web body unparseable JSON falls back to strip",
			// non-JSON body → fall back to envelope-strip path
			`{"_txc":{"web":{"req":{"body":"bm90LWpzb24="}}},"repoName":"r"}`,
			`{"repoName":"r"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractMCPArguments(tc.envelope)
			if !sameJSON(got, tc.want) {
				t.Errorf("extractMCPArguments(%s) = %s, want %s", tc.envelope, got, tc.want)
			}
		})
	}
}

func TestExecMCPHTTPInputPassedAsArguments(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	}
	pu, _ := newTestUnit(t)
	input := `{"q":"hello","limit":5}`
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "search", input, "")); err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	// last recorded body is tools/call.
	last := stub.bodies[len(stub.bodies)-1]
	if got := gjson.GetBytes(last, "params.name").String(); got != "search" {
		t.Errorf("params.name = %q, want 'search' (body=%s)", got, last)
	}
	args := gjson.GetBytes(last, "params.arguments").Raw
	if !sameJSON(args, input) {
		t.Errorf("params.arguments = %s, want %s", args, input)
	}
}

func TestExecMCPHTTPHonorsHeaderSecretOverlays(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	}

	op := operation.Operation{
		Input:     `{"q":"x"}`,
		Meta:      `{"secrets":{"headers":{"authorization":{"secret":"t","format":"Bearer {}"}}}}`,
		Resonator: &resonator.Resonator{Exec: mcpExecURL(stub.URL, "t")},
	}
	op.Secrets.Set("t", []byte("abc"))

	pu, _ := newTestUnit(t)
	if _, err := pu.ExecMCPHTTP(context.Background(), op); err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	// tools/call is the last request.
	last := stub.calls[len(stub.calls)-1]
	if got := last.Header.Get("Authorization"); got != "Bearer abc" {
		t.Errorf("tools/call Authorization = %q, want 'Bearer abc'", got)
	}
	// Authorization must NOT have been set on initialize / notif.
	for i, phase := range []string{"initialize", "notifications/initialized"} {
		if got := stub.calls[i].Header.Get("Authorization"); got != "" {
			t.Errorf("%s also carried Authorization=%q (overlay leaked to handshake)", phase, got)
		}
	}
	// Cleartext must not be inside the JSON-RPC body of tools/call.
	body := stub.bodies[len(stub.bodies)-1]
	if strings.Contains(string(body), "Bearer abc") {
		t.Errorf("cleartext bearer leaked into tools/call body: %s", body)
	}
	if strings.Contains(string(body), "secret://") {
		t.Errorf("secret ref literal leaked into tools/call body: %s", body)
	}
}

func TestExecMCPHTTPHandlesSSEResponse(t *testing.T) {
	// Spec-shaped streamable-HTTP server: responds with
	// Content-Type: text/event-stream wrapping the JSON-RPC reply
	// in a single `event: message` frame. DeepWiki (and others) do
	// this; v0 must transparently unwrap.
	var phase int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		id := gjson.GetBytes(body, "id").Int()
		phase++
		w.Header().Set("Content-Type", "text/event-stream")
		switch method {
		case "initialize":
			w.Header().Set(mcpSessionHeader, "sse-sess")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"protocolVersion\":\"%s\",\"serverInfo\":{\"name\":\"sse-stub\",\"version\":\"0\"}}}\n\n",
				id, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"sse hi\"}]}}\n\n", id)
		}
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpOp(srv.URL, "t", `{}`, ""))
	if err != nil {
		t.Fatalf("ExecMCPHTTP over SSE: %v", err)
	}
	if got := gjson.Get(payload.Raw, "text").String(); got != "sse hi" {
		t.Errorf("text projection from SSE = %q, want 'sse hi' (raw=%s)", got, payload.Raw)
	}
}

func TestExecMCPHTTPRejectsArgumentsSecretRef(t *testing.T) {
	// v0 contract: secrets.body.* refs are silent no-ops; the
	// literal "secret://NAME" stays in the serialized arguments.
	// Pins the deferred-by-design behavior so a future v0.5
	// enabling body overlays is detected as a behavior change.
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	}
	op := operation.Operation{
		Input:     `{"token":"placeholder"}`,
		Meta:      `{"secrets":{"body":{"token":{"secret":"t"}}}}`,
		Resonator: &resonator.Resonator{Exec: mcpExecURL(stub.URL, "t")},
	}
	op.Secrets.Set("t", []byte("CLEARTEXT-VALUE"))

	pu, _ := newTestUnit(t)
	if _, err := pu.ExecMCPHTTP(context.Background(), op); err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	last := stub.bodies[len(stub.bodies)-1]
	// Body cleartext must NOT have been substituted into the
	// outgoing request body (the v0 invariant — header overlays
	// only, body overlays are silent no-ops).
	if strings.Contains(string(last), "CLEARTEXT-VALUE") {
		t.Errorf("v0 substituted body secret (regression — see feedback_secret_blast_radius.md): %s", last)
	}
	// The original placeholder should pass through unchanged.
	args := gjson.GetBytes(last, "params.arguments.token").String()
	if args != "placeholder" {
		t.Errorf("placeholder substituted (body overlay should be no-op): %q (body=%s)", args, last)
	}
}

// --- helpers ---

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameJSON(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return string(ab) == string(bb)
}

// Touch secrets to keep the import live for the body-overlay test
// (whose call into ExecMCPHTTP exercises it indirectly via
// secrets.ParseRefs).
var _ = secrets.Ref{}
