package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestWithResolveFailureIsStrict pins the strict-by-default contract for a
// failing WITH value, matching SET PRE / SET POST / EMIT.
//
// Before the fix, a WITH key whose value failed to resolve set `withErr`,
// discarded EVERY resolved key, and dispatched the op with Meta "{}" — no
// timeout, no `mode`, no `redact`, and critically no `secrets.*`. A rule whose
// upstream credential rode a WITH ref would therefore send an unauthenticated
// request and read as a bad response rather than a broken rule.
//
// Only a FunctionCall can reach the failure branch: runtime.Resolve maps a
// missing PathRef to (nil, nil), so this is the documented strict-function
// contract — `&json` on a malformed body halts the resonator.
func TestWithResolveFailureIsStrict(t *testing.T) {
	var (
		mu   sync.Mutex
		body string
		hits int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body, hits = string(b), hits+1
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)

	// &json on a non-JSON literal errors, so the WITH clause cannot resolve.
	rule := `WITH timeout = &json("not json") EXEC "` + srv.URL + `/echo"`
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', '')`,
		"boot/with-fail", 0, rule); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	resCh := make(chan event.Payload, 1)
	done := make(chan error, 1)
	go func() { done <- pu.Run(context.Background(), `{"hello":"world"}`, "boot/with-fail/0", resCh) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}

	mu.Lock()
	got, n := body, hits
	mu.Unlock()

	if n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}
	// The dispatched input must carry the failure, naming the offending key —
	// not the original envelope, which would mean the op ran regardless.
	if !strings.Contains(got, "WITH timeout:") {
		t.Errorf("dispatched body = %s\nwant a failure payload naming the WITH key", got)
	}
	if strings.Contains(got, `"hello"`) {
		t.Errorf("dispatched body = %s\nwant the failure payload to REPLACE the envelope, not ride alongside it", got)
	}
}
