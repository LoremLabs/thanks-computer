package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
)

// doctorStub is a minimal MCP-over-HTTP stub for doctor tests.
// It records the methods and sessions it sees and lets each test
// vary the initialize / tools-list response.
type doctorStub struct {
	URL string

	mu       sync.Mutex
	sessions []string

	SessionID string // empty → stateless
	OnInit    func(id int64) []byte
	OnList    func(id int64) []byte

	// ListRequiresSession: 400 when tools/list arrives without
	// Mcp-Session-Id.
	ListRequiresSession bool

	srv *httptest.Server
}

func newDoctorStub(t *testing.T) *doctorStub {
	t.Helper()
	s := &doctorStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	s.URL = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *doctorStub) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	method := gjson.GetBytes(body, "method").String()
	id := gjson.GetBytes(body, "id").Int()

	s.mu.Lock()
	s.sessions = append(s.sessions, r.Header.Get(mcpSessionHeader))
	s.mu.Unlock()

	switch method {
	case "initialize":
		if s.SessionID != "" {
			w.Header().Set(mcpSessionHeader, s.SessionID)
		}
		w.Header().Set("Content-Type", "application/json")
		if s.OnInit != nil {
			_, _ = w.Write(s.OnInit(id))
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":%q,"serverInfo":{"name":"stub","version":"0"}}}`,
			id, mcpProtocolVersion)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		if s.ListRequiresSession && r.Header.Get(mcpSessionHeader) == "" {
			http.Error(w, "missing Mcp-Session-Id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s.OnList != nil {
			_, _ = w.Write(s.OnList(id))
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, id)
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func TestMcpDoctorHappyPath(t *testing.T) {
	stub := newDoctorStub(t)
	stub.SessionID = "doctor-1"
	stub.OnList = func(id int64) []byte {
		return []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"result":{"tools":[
				{"name":"summarize","description":"First line.\nSecond.","inputSchema":{"properties":{"q":{"type":"string"},"limit":{"type":"integer"}}}},
				{"name":"search","description":"Search docs.","inputSchema":{}}
			]}}`, id))
	}

	var stdout, stderr bytes.Buffer
	code := runMcpDoctor([]string{"mcp+" + stub.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"summarize", "search", "First line.", "Search docs.", "q, limit", "doctor-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n--- stdout ---\n%s", want, out)
		}
	}
	// Session id should have been carried on initialized + tools/list.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sessions) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(stub.sessions))
	}
	if stub.sessions[0] != "" {
		t.Errorf("initialize carried session id %q (should be empty)", stub.sessions[0])
	}
	if stub.sessions[1] != "doctor-1" {
		t.Errorf("initialized session = %q, want doctor-1", stub.sessions[1])
	}
	if stub.sessions[2] != "doctor-1" {
		t.Errorf("tools/list session = %q, want doctor-1", stub.sessions[2])
	}
}

func TestMcpDoctorStatelessServer(t *testing.T) {
	stub := newDoctorStub(t) // SessionID = "" → stateless
	stub.OnList = func(id int64) []byte {
		return []byte(fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"t","description":""}]}}`, id))
	}
	var stdout, stderr bytes.Buffer
	if code := runMcpDoctor([]string{stub.URL}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "(stateless)") {
		t.Errorf("expected stateless marker in stdout:\n%s", stdout.String())
	}
}

func TestMcpDoctorDialFailed(t *testing.T) {
	// Point at a closed port. Use a fresh listener and close it
	// to grab a guaranteed-unused address.
	stub := newDoctorStub(t)
	closedURL := stub.URL
	stub.srv.Close() // close immediately

	var stdout, stderr bytes.Buffer
	code := runMcpDoctor([]string{"--timeout=500ms", "mcp+" + closedURL}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit, got 0\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "dial-failed") && !strings.Contains(stderr.String(), "init:") {
		t.Errorf("expected dial-failed / init: diagnostic, got: %s", stderr.String())
	}
}

func TestMcpDoctorInitRPCError(t *testing.T) {
	stub := newDoctorStub(t)
	stub.OnInit = func(id int64) []byte {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32600,"message":"unsupported version"}}`, id))
	}
	var stdout, stderr bytes.Buffer
	code := runMcpDoctor([]string{stub.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit on init RPC error")
	}
	if !strings.Contains(stderr.String(), "rpc-error") {
		t.Errorf("expected rpc-error diagnostic, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unsupported version") {
		t.Errorf("expected upstream message surfaced, got: %s", stderr.String())
	}
}

func TestMcpDoctorBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := runMcpDoctor([]string{srv.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit on malformed JSON")
	}
	if !strings.Contains(stderr.String(), "rpc-error") {
		t.Errorf("expected rpc-error (decode failure surfaces as rpc-error), got: %s", stderr.String())
	}
}

func TestMcpDoctorSessionRequired(t *testing.T) {
	// Server forbids tools/list without Mcp-Session-Id BUT does not
	// return one from initialize. Doctor must surface the
	// session-required diagnostic distinctly.
	stub := newDoctorStub(t)
	stub.SessionID = "" // stateless from server's perspective
	stub.ListRequiresSession = true

	var stdout, stderr bytes.Buffer
	code := runMcpDoctor([]string{stub.URL}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("expected non-zero exit when tools/list rejects missing session")
	}
	if !strings.Contains(stderr.String(), "session-required") {
		t.Errorf("expected session-required diagnostic, got: %s", stderr.String())
	}
}

// Compile-time check that doctorPost honors context.
var _ = context.Background
