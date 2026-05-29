package jsonrpc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCallRoundTrip(t *testing.T) {
	params, _ := json.Marshal(map[string]any{"name": "summarize", "arguments": map[string]string{"k": "v"}})
	body, err := Call(7, "tools/call", params)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got Request
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", got.JSONRPC)
	}
	if got.ID != 7 {
		t.Errorf("id = %d, want 7", got.ID)
	}
	if got.Method != "tools/call" {
		t.Errorf("method = %q, want tools/call", got.Method)
	}
	if !strings.Contains(string(got.Params), `"summarize"`) {
		t.Errorf("params lost: %s", got.Params)
	}
}

func TestCallNoParams(t *testing.T) {
	body, err := Call(1, "ping", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Params should be absent on the wire (omitempty when zero-length).
	if strings.Contains(string(body), `"params"`) {
		t.Errorf("params present when nil: %s", body)
	}
}

func TestCallRejectsEmptyMethod(t *testing.T) {
	if _, err := Call(1, "", nil); err == nil {
		t.Fatal("Call accepted empty method, want error")
	}
}

func TestNotifyOmitsID(t *testing.T) {
	body, err := Notify("notifications/initialized", nil)
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if strings.Contains(string(body), `"id"`) {
		t.Errorf("notification carried id: %s", body)
	}
	if !strings.Contains(string(body), `"notifications/initialized"`) {
		t.Errorf("notification missing method: %s", body)
	}
}

func TestParseResult(t *testing.T) {
	r, err := Parse([]byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"hi"}]}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.ID != 7 {
		t.Errorf("id = %d, want 7", r.ID)
	}
	if r.Error != nil {
		t.Errorf("unexpected error: %v", r.Error)
	}
	if !strings.Contains(string(r.Result), `"hi"`) {
		t.Errorf("result lost: %s", r.Result)
	}
}

func TestParseSurfacesError(t *testing.T) {
	r, err := Parse([]byte(`{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"bad arg"}}`))
	if err == nil {
		t.Fatal("Parse accepted error response without error")
	}
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is not *jsonrpc.Error: %T (%v)", err, err)
	}
	if rpcErr.Code != -32602 || rpcErr.Message != "bad arg" {
		t.Errorf("error fields lost: %+v", rpcErr)
	}
	// The decoded response should still be available alongside the error.
	if r.Error == nil {
		t.Error("Parse returned err but Response.Error nil")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("Parse accepted garbage")
	}
}

func TestExtractSSESingleFrame(t *testing.T) {
	body := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n")
	got := ExtractSSE(body)
	if !strings.Contains(string(got), `"ok":true`) {
		t.Errorf("ExtractSSE = %q, want JSON-RPC payload", got)
	}
}

func TestExtractSSEDefaultEventName(t *testing.T) {
	// `event:` omitted → defaults to "message" per spec.
	body := []byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"v\":1}}\n\n")
	if got := ExtractSSE(body); !strings.Contains(string(got), `"v":1`) {
		t.Errorf("ExtractSSE = %q, want JSON-RPC payload (default event=message)", got)
	}
}

func TestExtractSSEMultiLineData(t *testing.T) {
	// Two data: lines join with newline; valid JSON pretty-printed
	// across lines should reassemble.
	body := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n")
	got := ExtractSSE(body)
	// The two data chunks are joined with '\n'. Don't assert exact
	// JSON — just that the prefix made it through.
	if !strings.Contains(string(got), `"jsonrpc":"2.0"`) {
		t.Errorf("ExtractSSE = %q, want concatenated payload", got)
	}
}

func TestExtractSSESkipsNonMessageEvents(t *testing.T) {
	// A leading "ping" event (no data) must not block the
	// subsequent message frame.
	body := []byte("event: ping\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"x\":1}}\n\n")
	if got := ExtractSSE(body); !strings.Contains(string(got), `"x":1`) {
		t.Errorf("ExtractSSE = %q, want message frame after ping", got)
	}
}

func TestExtractSSECRLF(t *testing.T) {
	// CRLF line endings (per spec) work the same as LF.
	body := []byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"x\":2}}\r\n\r\n")
	if got := ExtractSSE(body); !strings.Contains(string(got), `"x":2`) {
		t.Errorf("ExtractSSE CRLF = %q, want extracted payload", got)
	}
}

func TestExtractSSENoFrameReturnsNil(t *testing.T) {
	// Empty body / no data lines → nil. Caller surfaces this as a
	// decode error via Parse on next step.
	for _, b := range [][]byte{nil, []byte(""), []byte("\n\n"), []byte("event: ping\n\n")} {
		if got := ExtractSSE(b); got != nil {
			t.Errorf("ExtractSSE(%q) = %q, want nil", b, got)
		}
	}
}

// TestExtractSSESkipsNotificationsBeforeResponse — the prod bug.
// MCP servers (DeepWiki, et al.) interleave `notifications/message`
// log frames with the JSON-RPC response. Both arrive as
// `event: message` SSE frames. ExtractSSE must skip notification
// frames (which have `method` but no `id`/`result`) and return the
// actual response.
func TestExtractSSESkipsNotificationsBeforeResponse(t *testing.T) {
	// Mimics DeepWiki's wire shape: heartbeat comment, log
	// notification, then the real response.
	body := []byte(": ping\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":{\"msg\":\"Processing query… (15s elapsed)\"}}}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n\n")
	got := ExtractSSE(body)
	if !strings.Contains(string(got), `"result"`) {
		t.Fatalf("ExtractSSE returned a non-response frame: %q", got)
	}
	if strings.Contains(string(got), `"notifications/message"`) {
		t.Errorf("ExtractSSE returned a notification frame; should have skipped: %q", got)
	}
	if !strings.Contains(string(got), `"text":"hi"`) {
		t.Errorf("ExtractSSE didn't return the response text: %q", got)
	}
}

// TestExtractSSESkipsErrorOnlyFrame — JSON-RPC error responses
// (with `error` instead of `result`) are valid responses and must
// be extracted.
func TestExtractSSESkipsNotificationsBeforeErrorResponse(t *testing.T) {
	body := []byte(
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"percent\":50}}\n\n" +
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32602,\"message\":\"bad arg\"}}\n\n")
	got := ExtractSSE(body)
	if !strings.Contains(string(got), `"error"`) {
		t.Errorf("ExtractSSE didn't return the error response: %q", got)
	}
}
