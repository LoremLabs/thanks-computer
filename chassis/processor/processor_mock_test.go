package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// TestExecTxcoMockReturnsMockRes — when a rule's EXEC is "txco://mock",
// the runtime should return op.MockRes verbatim and NOT dispatch.
func TestExecTxcoMockReturnsMockRes(t *testing.T) {
	pu, _ := newTestUnit(t)

	rule := `WHEN .x == 1 EXEC "txco://mock"`
	mockRes := `{"mocked":true,"shape":"explicit"}`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		"mocktest/explicit", 0, "hello", rule, mockRes); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"x":1}`, "mocktest/explicit/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "mocked").Bool(); !got {
			t.Errorf("response missing mocked=true; body=%s", payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "shape").String(); got != "explicit" {
			t.Errorf("response shape = %q, want explicit; body=%s", got, payload.Raw)
		}
	default:
		t.Fatal("no response received")
	}
}

// TestExecTxcoMockEmptyMockResReturnsEmpty — txco://mock with empty
// mock_res is a rule-authoring mistake. The dispatch branch returns
// `{}` (and an internal error). Verify the body has no leaked content.
func TestExecTxcoMockEmptyMockResReturnsEmpty(t *testing.T) {
	pu, _ := newTestUnit(t)

	rule := `WHEN .x == 1 EXEC "txco://mock"`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"mocktest/empty", 0, "blank", rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"x":1}`, "mocktest/empty/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		if v := gjson.Get(payload.Raw, "mocked"); v.Exists() {
			t.Errorf("empty-mock-res case unexpectedly produced mocked field; body=%s", payload.Raw)
		}
	default:
		t.Fatal("no response received")
	}
}

// TestExecMocksPatternMatch — when `_txc.mocks` includes a pattern
// matching the firing op's identity AND op.MockRes is non-empty, the
// runtime substitutes the mock and does NOT hit the real HTTP target.
func TestExecMocksPatternMatch(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"real":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	rule := `WHEN .x == 1 EXEC "` + srv.URL + `/echo"`
	mockRes := `{"mocked":true,"shape":"pattern"}`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		"mocktest/pattern", 0, "hello", rule, mockRes); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	body := `{"x":1,"_txc":{"mocks":["mocktest/pattern/**"]}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), body, "mocktest/pattern/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("real HTTP target was hit %d time(s); expected 0", got)
	}
	select {
	case payload := <-resCh:
		if !gjson.Get(payload.Raw, "mocked").Bool() {
			t.Errorf("response missing mocked=true; body=%s", payload.Raw)
		}
	default:
		t.Fatal("no response received")
	}
}

// TestExecMocksPatternMatchOnNoExecRule — a rule with no EXEC clause
// should be treated as txco://noop and still pick up the _txc.mocks
// pattern interception. Without this symmetry the user has to add a
// dummy `EXEC "txco://noop"` to make their no-EXEC rule mockable, which
// is surprising.
func TestExecMocksPatternMatchOnNoExecRule(t *testing.T) {
	pu, _ := newTestUnit(t)

	// Pure comment rule (no WHEN, no EXEC). Without the symmetry fix
	// this rule's op never enters pu.Exec, so the mock interception
	// never gets a chance.
	rule := "# this rule has no EXEC\n"
	mockRes := `{"hihi":"hello"}`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		"mocktest/no-exec", 0, "hello", rule, mockRes); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	body := `{"_txc":{"mocks":["mocktest/no-exec/**"]}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), body, "mocktest/no-exec/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "hihi").String(); got != "hello" {
			t.Errorf("no-EXEC rule didn't pick up mock: hihi = %q, want %q; body=%s",
				got, "hello", payload.Raw)
		}
	default:
		t.Fatal("no response received")
	}
}

// TestExecMocksFallsThroughWhenMockResEmpty — pattern matches but
// mock_res is empty: the caller said "mock these" but the author
// didn't supply a fixture, so we'd rather run real than silently {}.
func TestExecMocksFallsThroughWhenMockResEmpty(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"real":true}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	rule := `EXEC "` + srv.URL + `/echo"`
	// mock_res deliberately empty.
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"mocktest/empty-mock", 0, "noop", rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	body := `{"_txc":{"mocks":["**"]}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), body, "mocktest/empty-mock/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("real HTTP target hit %d time(s); expected 1 (empty mock_res should fall through)", got)
	}
}

// TestShouldMockByPatternParsing — direct unit test of the helper.
// Covers string vs array shapes, exclusions, scope-specific patterns.
func TestShouldMockByPatternParsing(t *testing.T) {
	pu, _ := newTestUnit(t)

	cases := []struct {
		name    string
		body    string
		opStack string
		opScope int
		opName  string
		want    bool
	}{
		{"no _txc.mocks", `{}`, "hello-world", 100, "greet", false},
		{"string pattern matches", `{"_txc":{"mocks":"hello-world/**"}}`, "hello-world", 100, "greet", true},
		{"string pattern miss", `{"_txc":{"mocks":"other/**"}}`, "hello-world", 100, "greet", false},
		{"array pattern matches", `{"_txc":{"mocks":["hello-world/**"]}}`, "hello-world", 100, "greet", true},
		{"array empty", `{"_txc":{"mocks":[]}}`, "hello-world", 100, "greet", false},
		{"exclusion overrides catch-all", `{"_txc":{"mocks":["**","!hello-world/100/greet"]}}`, "hello-world", 100, "greet", false},
		{"exclusion misses unrelated op", `{"_txc":{"mocks":["**","!hello-world/100/greet"]}}`, "hello-world", 100, "other", true},
		{"scope-specific pattern", `{"_txc":{"mocks":["**/100/*"]}}`, "hello-world", 100, "greet", true},
		{"scope-specific pattern miss", `{"_txc":{"mocks":["**/100/*"]}}`, "hello-world", 200, "greet", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := operation.Operation{
				Input: tc.body,
				Stack: tc.opStack,
				Scope: tc.opScope,
				Name:  tc.opName,
			}
			got := pu.shouldMockByPattern(op)
			if got != tc.want {
				t.Errorf("shouldMockByPattern(%s/%d/%s, body=%s) = %v, want %v",
					tc.opStack, tc.opScope, tc.opName, tc.body, got, tc.want)
			}
		})
	}
}
