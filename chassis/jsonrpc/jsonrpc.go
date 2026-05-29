// Package jsonrpc is an internal framing helper for JSON-RPC 2.0
// requests and responses. It is the private abstraction underneath
// the `mcp+http(s)://` EXEC scheme and the `txco mcp doctor` CLI;
// it deliberately exposes no transport, no client, no policy.
//
// Per internal docs/todo-mcp.md §8 there is no public `json-rpc+https://`
// scheme. Callers that need one in the future promote this package
// into a scheme rather than re-extracting the framing.
package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Request models a JSON-RPC 2.0 request or notification.
// A notification omits ID by serializing with ID == 0 — callers
// that need true notification semantics (no response) should use
// Notification instead, which omits the field entirely.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Notification models a JSON-RPC 2.0 notification (no `id`,
// no response). Used for `notifications/initialized` and the
// other `notifications/*` methods.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the decoded JSON-RPC 2.0 reply.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the wire-shape JSON-RPC error object plus implements
// the Go error interface so callers handle a Result.Error returned
// by Parse with the same `if err != nil` idiom as a transport
// error.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Call marshals a single JSON-RPC 2.0 request with a numeric id.
// params may be nil (no params) or a pre-encoded JSON value
// (object or array).
func Call(id int, method string, params []byte) ([]byte, error) {
	if method == "" {
		return nil, errors.New("jsonrpc: empty method")
	}
	req := Request{JSONRPC: "2.0", ID: id, Method: method}
	if len(params) > 0 {
		req.Params = json.RawMessage(params)
	}
	return json.Marshal(req)
}

// Notify marshals a JSON-RPC 2.0 notification (no `id`, no
// response expected on the wire).
func Notify(method string, params []byte) ([]byte, error) {
	if method == "" {
		return nil, errors.New("jsonrpc: empty method")
	}
	n := Notification{JSONRPC: "2.0", Method: method}
	if len(params) > 0 {
		n.Params = json.RawMessage(params)
	}
	return json.Marshal(n)
}

// Parse decodes a JSON-RPC response body. Returns the decoded
// Response and a non-nil error when the body is not valid JSON
// or when the server returned a JSON-RPC error object (so callers
// handle both shapes uniformly).
func Parse(body []byte) (Response, error) {
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		return Response{}, fmt.Errorf("jsonrpc: decode: %w", err)
	}
	if r.Error != nil {
		return r, r.Error
	}
	return r, nil
}

// ExtractSSE pulls the first complete `event: message` frame's
// data from a Server-Sent Events body and returns the JSON-RPC
// payload it carries. MCP streamable-HTTP servers may respond to
// a single POST with either `application/json` (callers use
// Parse directly) or `text/event-stream` (callers extract via
// this helper first, then Parse).
//
// SSE framing per https://html.spec.whatwg.org/multipage/server-sent-events.html:
// lines beginning `event:` set the event name (default "message");
// `data:` lines are joined with newlines; a blank line dispatches
// a frame. This helper handles only the request/response case
// (one frame, one event); long-lived streams are out of scope.
//
// Returns an empty byte slice with no error if no `message` frame
// is found — the caller then surfaces it as a JSON decode error
// from Parse, which is consistent with other malformed-body
// behavior.
func ExtractSSE(body []byte) []byte {
	var (
		event   = "message"
		dataBuf []byte
	)
	// Walk lines. CR / LF / CRLF all delimit per the spec; using a
	// simple split on \n and trimming \r covers all three.
	//
	// MCP servers commonly interleave `notifications/*` (progress,
	// logging) frames with the actual JSON-RPC response — both arrive
	// as `event: message` frames. We must return the RESPONSE frame
	// (has `result` or `error`, with `id`), not the first notification
	// (has `method`, no `id`). isJSONRPCResponseFrame filters.
	dispatch := func() []byte {
		if event == "message" && len(dataBuf) > 0 && isJSONRPCResponseFrame(dataBuf) {
			return dataBuf
		}
		return nil
	}
	for _, raw := range bytes.Split(body, []byte{'\n'}) {
		line := bytes.TrimRight(raw, "\r")
		switch {
		case len(line) == 0:
			if out := dispatch(); out != nil {
				return out
			}
			// Reset for the next frame.
			event = "message"
			dataBuf = nil
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimPrefix(line, []byte("data:"))
			chunk = bytes.TrimPrefix(chunk, []byte(" ")) // optional leading space per spec
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, chunk...)
		default:
			// Comments (lines starting with ":") and other field
			// names ("id:", "retry:") are ignored for our purposes.
		}
	}
	// Body ended without a blank-line dispatch — accept the
	// trailing frame if it's a JSON-RPC response.
	return dispatch()
}

// isJSONRPCResponseFrame reports whether the data payload of an SSE
// frame is a JSON-RPC response (has top-level `result` or `error` —
// per the JSON-RPC 2.0 spec these are mutually exclusive with the
// `method` field that notifications/requests use). Notification
// frames like `{"method":"notifications/message","params":{…}}` are
// rejected so the response extractor can find the real reply even
// when the server interleaves log/progress notifications before it.
func isJSONRPCResponseFrame(data []byte) bool {
	// Quick byte-level checks before full JSON parse: response frames
	// always contain `"result"` or `"error"` AND `"id"`. This is
	// adequate for the tight read/write loop we're in; an actual
	// parse happens in Parse() right after this returns true.
	hasResultOrError := bytes.Contains(data, []byte(`"result"`)) ||
		bytes.Contains(data, []byte(`"error"`))
	if !hasResultOrError {
		return false
	}
	// Also require `"id"` — notifications have no id, and a "result"
	// key occurring inside a notification's params shouldn't fool us.
	return bytes.Contains(data, []byte(`"id"`))
}
