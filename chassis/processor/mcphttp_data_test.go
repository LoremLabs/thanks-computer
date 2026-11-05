package processor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// The data-driven forms of the MCP op (WITH url / tool / args_path / into /
// method), a credential on every phase, sessions per credential, and a
// failure as data. The literal-EXEC forms are mcphttp_test.go's.

// mcpDataExec is the literal a data-driven rule writes: the scheme
// dispatches here, and WITH url names the endpoint.
const mcpDataExec = "mcp+https://invalid/"

func mcpDataOp(input, meta string) operation.Operation {
	return operation.Operation{
		Name:      "call",
		Stack:     "tools-mcp",
		Input:     input,
		Meta:      meta,
		Resonator: &resonator.Resonator{Exec: mcpDataExec},
	}
}

func textResult(s string) func([]byte) []byte {
	return func(_ []byte) []byte {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":%q}]}}`, s))
	}
}

func (s *mcpStub) lastBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bodies) == 0 {
		return nil
	}
	return s.bodies[len(s.bodies)-1]
}

func (s *mcpStub) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.methods)
}

func (s *mcpStub) header(i int, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i >= len(s.calls) {
		return ""
	}
	return s.calls[i].Header.Get(name)
}

func TestExecMCPHTTPURLAndToolFromWith(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = textResult("hi")
	pu, _ := newTestUnit(t)

	meta := fmt.Sprintf(`{"url":%q,"tool":"search"}`, stub.URL)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{"q":"x"}`, meta))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.Get(payload.Raw, "text").String(); got != "hi" {
		t.Errorf("text = %q, want hi (raw=%s)", got, payload.Raw)
	}
	if got := gjson.GetBytes(stub.lastBody(), "params.name").String(); got != "search" {
		t.Errorf("params.name = %q, want search", got)
	}
	want := []string{"initialize", "notifications/initialized", "tools/call"}
	if got := stub.recordedMethods(); !equalSlices(got, want) {
		t.Errorf("methods = %v, want %v", got, want)
	}
}

func TestExecMCPHTTPURLOverrideForms(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = textResult("ok")
	pu, _ := newTestUnit(t)

	// The mcp+ prefix is allowed, and a #fragment names the tool.
	meta := fmt.Sprintf(`{"url":%q}`, "mcp+"+stub.URL+"#fromfrag")
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta)); err != nil {
		t.Fatalf("mcp+ override: %v", err)
	}
	if got := gjson.GetBytes(stub.lastBody(), "params.name").String(); got != "fromfrag" {
		t.Errorf("tool from the override's fragment = %q, want fromfrag", got)
	}

	// WITH tool wins over a fragment.
	meta = fmt.Sprintf(`{"url":%q,"tool":"explicit"}`, stub.URL+"#fromfrag")
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta)); err != nil {
		t.Fatalf("tool override: %v", err)
	}
	if got := gjson.GetBytes(stub.lastBody(), "params.name").String(); got != "explicit" {
		t.Errorf("WITH tool = %q, want explicit", got)
	}
}

func TestExecMCPHTTPURLOverrideSchemeLocked(t *testing.T) {
	stub := newMCPStub(t)
	pu, _ := newTestUnit(t)
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/mcp", "mcp+file:///x", "gopher://x"} {
		meta := fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, bad)
		_, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
		if err == nil || !strings.Contains(err.Error(), "WITH url override must be") {
			t.Errorf("url %q: err = %v, want the scheme refusal (an authoring error even with into)", bad, err)
		}
	}
	if n := stub.requestCount(); n != 0 {
		t.Errorf("a refused override sent %d requests", n)
	}
}

func TestExecMCPHTTPArgsPath(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = textResult("ok")
	pu, _ := newTestUnit(t)

	input := `{"_call":{"args":{"repo":"a/b","n":2}},"other":"not for the server","_txc":{"rid":"r1"}}`
	meta := fmt.Sprintf(`{"url":%q,"tool":"t","args_path":"_call.args"}`, stub.URL)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(input, meta)); err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.GetBytes(stub.lastBody(), "params.arguments").Raw; !sameJSON(got, `{"repo":"a/b","n":2}`) {
		t.Errorf("arguments = %s, want exactly the value at args_path", got)
	}

	before := stub.requestCount()
	// A path with nothing at it: an authoring error, nothing sent.
	meta = fmt.Sprintf(`{"url":%q,"tool":"t","args_path":"_call.nope","into":"_res"}`, stub.URL)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(input, meta)); err == nil || !strings.Contains(err.Error(), "names nothing") {
		t.Errorf("missing args_path: err = %v, want 'names nothing'", err)
	}
	// A value that is not an object.
	meta = fmt.Sprintf(`{"url":%q,"tool":"t","args_path":"other"}`, stub.URL)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(input, meta)); err == nil || !strings.Contains(err.Error(), "is not an object") {
		t.Errorf("non-object args_path: err = %v, want 'is not an object'", err)
	}
	if n := stub.requestCount(); n != before {
		t.Errorf("a refused args_path sent %d requests", n-before)
	}
}

func TestExecMCPHTTPIntoNestsAnswer(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = textResult("hi")
	pu, _ := newTestUnit(t)

	meta := fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, stub.URL)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_res.text").String(); got != "hi" {
		t.Errorf("_res.text = %q, want hi (raw=%s)", got, payload.Raw)
	}
	if gjson.Get(payload.Raw, "text").Exists() {
		t.Errorf("the answer also sits at the root: %s", payload.Raw)
	}
	if got := gjson.Get(payload.Raw, "_txc._ops.call.status").Int(); got != 200 {
		t.Errorf("_txc._ops.call.status = %d, want 200", got)
	}
}

func TestExecMCPHTTPStructuredContentAndIsError(t *testing.T) {
	stub := newMCPStub(t)
	pu, _ := newTestUnit(t)
	meta := fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, stub.URL)

	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"a\":1}"}],"structuredContent":{"a":1}}}`)
	}
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("structured: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_res.structuredContent.a").Int(); got != 1 {
		t.Errorf("structuredContent not kept: %s", payload.Raw)
	}
	if gjson.Get(payload.Raw, "_res.isError").Exists() {
		t.Errorf("isError set on a success: %s", payload.Raw)
	}

	// A tool that reports its own failure still answered: data, no Go error.
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"no such repo"}],"isError":true}}`)
	}
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("isError: %v", err)
	}
	if !gjson.Get(payload.Raw, "_res.isError").Bool() || gjson.Get(payload.Raw, "_res.text").String() != "no such repo" {
		t.Errorf("isError result = %s", payload.Raw)
	}
	if !gjson.Get(payload.Raw, "_txc._ops.call.is_error").Bool() {
		t.Errorf("_txc._ops.call.is_error not set: %s", payload.Raw)
	}
	if gjson.Get(payload.Raw, "_res.error").Exists() {
		t.Errorf("a tool's own failure is not an op failure: %s", payload.Raw)
	}
}

func TestExecMCPHTTPFailureIsDataWithInto(t *testing.T) {
	pu, _ := newTestUnit(t)

	// 401 at initialize: the server wants a credential.
	unauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.com/.well-known/oauth-protected-resource"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(unauth.Close)
	meta := fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, unauth.URL)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("401 with into: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_mcp_http" {
		t.Errorf("401 code = %q, want txco_mcp_http (raw=%s)", got, payload.Raw)
	}
	if got := gjson.Get(payload.Raw, "_res.error.status").Int(); got != 401 {
		t.Errorf("401 status = %d", got)
	}
	if got := gjson.Get(payload.Raw, "_res.error.phase").String(); got != "init" {
		t.Errorf("401 phase = %q, want init", got)
	}
	// Without into it is the Go error it always was.
	meta = fmt.Sprintf(`{"url":%q,"tool":"t"}`, unauth.URL)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta)); err == nil {
		t.Errorf("401 without into: want a Go error")
	}

	// A JSON-RPC error on tools/call.
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"repoName is required"}}`)
	}
	meta = fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, stub.URL)
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("rpc error with into: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_mcp_rpc" {
		t.Errorf("rpc code = %q, want txco_mcp_rpc (raw=%s)", got, payload.Raw)
	}
	if got := gjson.Get(payload.Raw, "_res.error.rpc_code").Int(); got != -32602 {
		t.Errorf("rpc_code = %d, want -32602", got)
	}
	if got := gjson.Get(payload.Raw, "_res.error.phase").String(); got != "call" {
		t.Errorf("rpc phase = %q, want call", got)
	}

	// Nothing listening: the dial.
	meta = `{"url":"http://127.0.0.1:1/mcp","tool":"t","into":"_res"}`
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("dial failure with into: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_mcp_dial" {
		t.Errorf("dial code = %q, want txco_mcp_dial (raw=%s)", got, payload.Raw)
	}
	if gjson.Get(payload.Raw, "_res.error.status").Exists() {
		t.Errorf("a dial failure has no status: %s", payload.Raw)
	}

	// An answer that is not JSON-RPC.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	t.Cleanup(junk.Close)
	meta = fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res"}`, junk.URL)
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("junk with into: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_mcp_bad_answer" {
		t.Errorf("junk code = %q, want txco_mcp_bad_answer (raw=%s)", got, payload.Raw)
	}
}

// pagedTools is an MCP server whose tools/list pages: each page holds
// `per` tools and names a next cursor until `pages` have been served
// (pages <= 0: forever).
func pagedTools(t *testing.T, per, pages int) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	cursors := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		id := gjson.GetBytes(body, "id").Int()
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"serverInfo":{"name":"paged","version":"0"}}}`, id, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			cursor := gjson.GetBytes(body, "params.cursor").String()
			mu.Lock()
			cursors = append(cursors, cursor)
			mu.Unlock()
			page := 0
			if cursor != "" {
				fmt.Sscanf(cursor, "p%d", &page)
			}
			var tools []string
			for i := 0; i < per; i++ {
				tools = append(tools, fmt.Sprintf(`{"name":"t%d_%d","description":"tool %d of page %d","inputSchema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"annotations":{"readOnlyHint":true}}`, page, i, i, page))
			}
			next := ""
			if pages <= 0 || page+1 < pages {
				next = fmt.Sprintf(`,"nextCursor":"p%d"`, page+1)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[%s]%s}}`, id, strings.Join(tools, ","), next)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &cursors
}

func TestExecMCPHTTPToolsList(t *testing.T) {
	pu, _ := newTestUnit(t)

	// Two pages, followed by cursor; no tool is named.
	srv, cursors := pagedTools(t, 2, 2)
	meta := fmt.Sprintf(`{"url":%q,"method":"tools/list","into":"_tl"}`, srv.URL)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{"ignored":"input"}`, meta))
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_tl.tools.#").Int(); got != 4 {
		t.Fatalf("tools = %d, want 4 (raw=%s)", got, payload.Raw)
	}
	if got := gjson.Get(payload.Raw, "_tl.tools.2.name").String(); got != "t1_0" {
		t.Errorf("third tool = %q, want t1_0 (page order kept)", got)
	}
	// Each tool as the server wrote it.
	if got := gjson.Get(payload.Raw, "_tl.tools.0.inputSchema.required.0").String(); got != "q" {
		t.Errorf("inputSchema not kept verbatim: %s", payload.Raw)
	}
	if !gjson.Get(payload.Raw, "_tl.tools.0.annotations.readOnlyHint").Bool() {
		t.Errorf("annotations not kept: %s", payload.Raw)
	}
	if gjson.Get(payload.Raw, "_tl.truncated").Exists() {
		t.Errorf("truncated set on a complete list")
	}
	if got := *cursors; len(got) != 2 || got[0] != "" || got[1] != "p1" {
		t.Errorf("cursors asked = %v, want [\"\" p1]", got)
	}

	// A server that never ends: mcpToolsListMax tools, and said so.
	endless, asked := pagedTools(t, 150, 0)
	meta = fmt.Sprintf(`{"url":%q,"method":"tools/list","into":"_tl"}`, endless.URL)
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("endless tools/list: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_tl.tools.#").Int(); got != mcpToolsListMax {
		t.Errorf("tools = %d, want %d", got, mcpToolsListMax)
	}
	if !gjson.Get(payload.Raw, "_tl.truncated").Bool() {
		t.Errorf("truncated not set: %s", gjson.Get(payload.Raw, "_tl.truncated").Raw)
	}
	if len(*asked) != 2 {
		t.Errorf("pages asked = %d, want 2", len(*asked))
	}

	// Without into the list is the answer.
	meta = fmt.Sprintf(`{"url":%q,"method":"tools/list"}`, srv.URL)
	payload, err = pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil || gjson.Get(payload.Raw, "tools.#").Int() != 4 {
		t.Errorf("tools/list without into: err=%v raw=%s", err, payload.Raw)
	}

	// Any other method is an authoring error.
	meta = fmt.Sprintf(`{"url":%q,"method":"resources/list","into":"_tl"}`, srv.URL)
	if _, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta)); err == nil || !strings.Contains(err.Error(), "WITH method must be") {
		t.Errorf("bad method: err = %v", err)
	}
}

func TestExecMCPHTTPToolsListTooLarge(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.OpPayloadMax = 400 // the handshake fits; a page of tools does not
	srv, _ := pagedTools(t, 20, 1)
	meta := fmt.Sprintf(`{"url":%q,"method":"tools/list","into":"_tl"}`, srv.URL)
	payload, err := pu.ExecMCPHTTP(context.Background(), mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("too large with into: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_tl.error.code").String(); got != "txco_mcp_too_large" {
		t.Errorf("code = %q, want txco_mcp_too_large (raw=%s)", got, payload.Raw)
	}
}

// TestSessionCachePerCredential — two calls to one endpoint with different
// header secrets each run their own handshake; the same secret reuses its
// session; a call with none has its own.
func TestSessionCachePerCredential(t *testing.T) {
	stub := newMCPStub(t)
	stub.SessionID = "sid"
	stub.OnToolsCall = textResult("ok")
	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)
	ctx := withCachedTenant(context.Background(), "tenant-a")

	withSecret := func(name string) operation.Operation {
		op := mcpDataOp(`{}`, fmt.Sprintf(`{"url":%q,"tool":"t","secrets":{"headers":{"authorization":{"secret":%q,"format":"Bearer {}"}}}}`, stub.URL, name))
		op.Secrets.Set(name, []byte("value-of-"+name))
		return op
	}
	for _, name := range []string{"k1", "k2", "k1"} {
		if _, err := pu.ExecMCPHTTP(ctx, withSecret(name)); err != nil {
			t.Fatalf("call with %s: %v", name, err)
		}
	}
	if _, err := pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, fmt.Sprintf(`{"url":%q,"tool":"t"}`, stub.URL))); err != nil {
		t.Fatalf("call with no credential: %v", err)
	}
	want := []string{
		"initialize", "notifications/initialized", "tools/call", // k1
		"initialize", "notifications/initialized", "tools/call", // k2
		"tools/call",                                            // k1 again: its session
		"initialize", "notifications/initialized", "tools/call", // none
	}
	if got := stub.recordedMethods(); !equalSlices(got, want) {
		t.Errorf("methods = %v\nwant     %v", got, want)
	}
}

// mcpFakeConnections is the connection service as the op sees it.
type mcpFakeConnections struct {
	mu      sync.Mutex
	uses    []connection.UseInput
	tokens  map[string]string // principal → token
	err     *connection.Error
	clients int
}

func (f *mcpFakeConnections) Use(_ context.Context, in connection.UseInput) (connection.UseResult, *connection.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uses = append(f.uses, in)
	if f.err != nil {
		return connection.UseResult{}, f.err
	}
	tok := f.tokens[in.Principal]
	return connection.UseResult{Header: "authorization", Value: "Bearer " + tok, Token: tok}, nil
}

func (f *mcpFakeConnections) HTTPClient(_ *url.URL) *http.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients++
	return &http.Client{Timeout: 5 * time.Second}
}

func connMeta(stubURL, extra string) string {
	return fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res","secrets":{"headers":{"authorization":{"connection":"mcp:linear"%s}}}}`, stubURL, extra)
}

func TestExecMCPHTTPConnectionOnEveryPhase(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = textResult("ok")
	pu, _ := newTestUnit(t)
	fake := &mcpFakeConnections{tokens: map[string]string{"pony:a": "tok-a"}}
	pu.Connections = fake

	ctx := WithCapAuthority(withCachedTenant(context.Background(), "tenant-a"), "pony:a", "mcp.call")
	payload, err := pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, connMeta(stub.URL, "")))
	if err != nil {
		t.Fatalf("ExecMCPHTTP: %v", err)
	}
	if got := gjson.Get(payload.Raw, "_res.text").String(); got != "ok" {
		t.Fatalf("answer = %s", payload.Raw)
	}
	for i, phase := range []string{"initialize", "notifications/initialized", "tools/call"} {
		if got := stub.header(i, "Authorization"); got != "Bearer tok-a" {
			t.Errorf("%s Authorization = %q, want 'Bearer tok-a'", phase, got)
		}
	}
	if len(fake.uses) != 1 {
		t.Fatalf("Use called %d times, want 1", len(fake.uses))
	}
	in := fake.uses[0]
	if in.Tenant != "tenant-a" || in.Stack != "tools-mcp" || in.Provider != "mcp:linear" || in.Principal != "pony:a" || in.Capability != "mcp.call" {
		t.Errorf("UseInput = %+v", in)
	}
	if in.URL == nil || "http://"+in.URL.Host != stub.URL {
		t.Errorf("UseInput.URL = %v, want the endpoint %s", in.URL, stub.URL)
	}
	if fake.clients == 0 {
		t.Errorf("the connection service's client was not used")
	}
	// The token is in no payload.
	if strings.Contains(payload.Raw, "tok-a") {
		t.Errorf("the token is in the answer: %s", payload.Raw)
	}

	// The op's own format wins.
	if _, err := pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, connMeta(stub.URL, `,"format":"Token {}"`))); err != nil {
		t.Fatalf("format: %v", err)
	}
	if got := stub.header(stub.requestCount()-1, "Authorization"); got != "Token tok-a" {
		t.Errorf("formatted Authorization = %q, want 'Token tok-a'", got)
	}
}

func TestExecMCPHTTPConnectionSessionsPerPrincipal(t *testing.T) {
	stub := newMCPStub(t)
	stub.SessionID = "sid"
	stub.OnToolsCall = textResult("ok")
	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)
	pu.Connections = &mcpFakeConnections{tokens: map[string]string{"pony:a": "tok-a", "pony:b": "tok-b"}}

	base := withCachedTenant(context.Background(), "tenant-a")
	for _, who := range []string{"pony:a", "pony:b", "pony:a"} {
		ctx := WithCapAuthority(base, who, "mcp.call")
		if _, err := pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, connMeta(stub.URL, ""))); err != nil {
			t.Fatalf("%s: %v", who, err)
		}
	}
	want := []string{
		"initialize", "notifications/initialized", "tools/call", // a
		"initialize", "notifications/initialized", "tools/call", // b: its own session
		"tools/call", // a again
	}
	if got := stub.recordedMethods(); !equalSlices(got, want) {
		t.Errorf("methods = %v\nwant     %v", got, want)
	}
	// b's handshake carried b's token, never a's.
	if got := stub.header(3, "Authorization"); got != "Bearer tok-b" {
		t.Errorf("b's initialize Authorization = %q", got)
	}
}

func TestExecMCPHTTPConnectionRefusedIsData(t *testing.T) {
	stub := newMCPStub(t)
	pu, _ := newTestUnit(t)
	ctx := WithCapAuthority(withCachedTenant(context.Background(), "tenant-a"), "pony:a", "mcp.call")

	// No connection service on this node.
	payload, err := pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, connMeta(stub.URL, "")))
	if err != nil {
		t.Fatalf("nil service: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_connection_unavailable" {
		t.Errorf("nil service code = %q (raw=%s)", got, payload.Raw)
	}

	// The principal has not connected.
	pu.Connections = &mcpFakeConnections{err: &connection.Error{Code: connection.CodeMissing, Message: "not connected"}}
	payload, err = pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, connMeta(stub.URL, "")))
	if err != nil {
		t.Fatalf("missing: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_connection_missing" {
		t.Errorf("missing code = %q (raw=%s)", got, payload.Raw)
	}
	if got := gjson.Get(payload.Raw, "_res.error.connection").String(); got != "mcp:linear" {
		t.Errorf("error.connection = %q", got)
	}

	// A connection on anything but a header.
	pu.Connections = &mcpFakeConnections{tokens: map[string]string{"pony:a": "tok-a"}}
	meta := fmt.Sprintf(`{"url":%q,"tool":"t","into":"_res","secrets":{"body":{"token":{"connection":"mcp:linear"}}}}`, stub.URL)
	payload, err = pu.ExecMCPHTTP(ctx, mcpDataOp(`{}`, meta))
	if err != nil {
		t.Fatalf("body connection: err = %v, want data", err)
	}
	if got := gjson.Get(payload.Raw, "_res.error.code").String(); got != "txco_connection_invalid_arg" {
		t.Errorf("body connection code = %q (raw=%s)", got, payload.Raw)
	}

	if n := stub.requestCount(); n != 0 {
		t.Errorf("a refused connection sent %d requests", n)
	}
}
