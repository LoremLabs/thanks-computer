package processor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/tidwall/gjson"
)

// TestContinuation202EndsOpenStream: a promotion after the response started
// streaming (a body written in a non-terminal scope: StreamHead + chunk
// already sent, the head locked) ENDS the stream — the client has its
// answer — instead of emitting a 202 the stream writers would drop, which
// left the request open to its deadline (www.onepony.com, 2026-10-11).
func TestContinuation202EndsOpenStream(t *testing.T) {
	pu, _ := newTestUnit(t)
	raw := `{"_txc":{"tenant":"t1","runtime":{"http_stream_open":true},` +
		`"web":{"req":{"url":{"path":"/go"},"headers":{"Accept":["application/json"]}},` +
		`"res":{"status":200,"headers":{"content-type":["application/json"]}}}}}`

	resCh := make(chan event.Payload, 1)
	pu.emitContinuation202(context.Background(), raw, "rc_1", resCh)
	got := <-resCh
	if got.Type != event.StreamEnd {
		t.Fatalf("payload type = %v, want StreamEnd", got.Type)
	}
	// The terminator carries the envelope (the tee bills from it), with the
	// stack's own answer untouched: no 202, no poll Location.
	if s := gjson.Get(got.Raw, "_txc.web.res.status").Int(); s != 200 {
		t.Errorf("status = %d, want the stack's 200 left alone", s)
	}
	if loc := gjson.Get(got.Raw, "_txc.web.res.headers.location"); loc.Exists() {
		t.Errorf("location = %s, want none", loc.Raw)
	}
	if ten := gjson.Get(got.Raw, "_txc.tenant").String(); ten != "t1" {
		t.Errorf("tenant = %q, want the envelope's", ten)
	}
}

// TestContinuation202WhenNotStreaming: without an open stream the 202 is
// emitted as before (the JSON "still running" answer with its poll URL).
func TestContinuation202WhenNotStreaming(t *testing.T) {
	pu, _ := newTestUnit(t)
	raw := `{"_txc":{"web":{"req":{"url":{"path":"/go"},"headers":{"Accept":["application/json"]}},` +
		`"res":{"status":200}}}}`

	resCh := make(chan event.Payload, 1)
	pu.emitContinuation202(context.Background(), raw, "rc_1", resCh)
	got := <-resCh
	if got.Type != event.JSON {
		t.Fatalf("payload type = %v, want JSON", got.Type)
	}
	if s := gjson.Get(got.Raw, "_txc.web.res.status").Int(); s != 202 {
		t.Errorf("status = %d, want 202", s)
	}
}

// TestFailureAfterEarlyAnswerEndsStream: a body answered in a non-terminal
// scope streams (head + chunk), and a failing op in a LATER scope must end
// that stream — the same hang as the promotion's otherwise: an ErrorStr
// the stream writers drop, the request open to its deadline. The error
// rides on the terminator's envelope.
func TestFailureAfterEarlyAnswerEndsStream(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Computes = &stubRunner{err: errors.New("boom-after-answer")}
	seedOp(t, pu, "site", 100, "answer", `EMIT @web.res.status = 200, @web.res.body = b64"ok"`)
	seedOp(t, pu, "site", 200, "fail", `EXEC "compute://sha256/deadbeef"`)

	resCh := make(chan event.Payload, 8)
	go func() { _ = pu.Run(context.Background(), `{}`, "site/100", resCh) }()

	var types []event.Type
	var last event.Payload
	deadline := time.After(5 * time.Second)
	for len(types) == 0 || types[len(types)-1] != event.StreamEnd {
		select {
		case p := <-resCh:
			types = append(types, p.Type)
			last = p
			if p.Type == event.ErrorStr {
				t.Fatalf("got an ErrorStr on an open stream (payloads %v); want the stream ended", types)
			}
		case <-deadline:
			t.Fatalf("no StreamEnd within 5s; payloads so far %v", types)
		}
	}
	if types[0] != event.StreamHead {
		t.Errorf("first payload = %v, want StreamHead", types[0])
	}
	if msg := gjson.Get(last.Raw, "error.message").String(); msg != "boom-after-answer" {
		t.Errorf("terminator error.message = %q, want the failure", msg)
	}
}
