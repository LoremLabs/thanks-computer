package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// drain collects every payload currently buffered on resCh. Safe to call
// after Run returns: all stream messages are buffered (the channel is
// sized large enough that synchronous Run never blocks).
func drain(resCh chan event.Payload) []event.Payload {
	var out []event.Payload
	for {
		select {
		case p := <-resCh:
			out = append(out, p)
		default:
			return out
		}
	}
}

// TestRunStreamsBodyAcrossScopes locks in the streaming contract: a
// `_txc.web.res.body` written in a non-terminal scope is flushed as a
// chunk (preceded by a one-time StreamHead carrying status+headers), and
// the terminal scope's body is the final chunk followed by StreamEnd.
func TestRunStreamsBodyAcrossScopes(t *testing.T) {
	pu, _ := newTestUnit(t)
	// Scope 100: open the response, flush the first chunk, keep running.
	seedOp(t, pu, "svc", 100, "first",
		`EMIT @web.res.status = 200, @web.res.headers.content-type.0 = "text/plain", @web.res.body = b64"a\n"`)
	// Scope 200: final chunk + halt → terminal.
	seedOp(t, pu, "svc", 200, "second",
		`EMIT @web.res.body = b64"b\n", @halt = true`)

	resCh := make(chan event.Payload, 16)
	if err := pu.Run(context.Background(), `{}`, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := drain(resCh)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4 (head, chunk, chunk, end); msgs=%+v", len(msgs), msgs)
	}

	if msgs[0].Type != event.StreamHead {
		t.Errorf("msg[0].Type = %v, want StreamHead", msgs[0].Type)
	}
	if got := gjson.Get(msgs[0].Raw, "_txc.web.res.status").Int(); got != 200 {
		t.Errorf("head status = %d, want 200", got)
	}
	if got := gjson.Get(msgs[0].Raw, "_txc.web.res.headers.content-type.0").String(); got != "text/plain" {
		t.Errorf("head content-type = %q, want text/plain", got)
	}

	if msgs[1].Type != event.StreamChunk || msgs[1].Raw != "a\n" {
		t.Errorf("msg[1] = {%v %q}, want {StreamChunk \"a\\n\"}", msgs[1].Type, msgs[1].Raw)
	}
	if msgs[2].Type != event.StreamChunk || msgs[2].Raw != "b\n" {
		t.Errorf("msg[2] = {%v %q}, want {StreamChunk \"b\\n\"}", msgs[2].Type, msgs[2].Raw)
	}
	if msgs[3].Type != event.StreamEnd {
		t.Errorf("msg[3].Type = %v, want StreamEnd", msgs[3].Type)
	}
}

// TestRunBufferedBodyWithHaltDoesNotStream is the zero-regression guard:
// the dominant pattern (body + @halt in the same, terminal scope) must
// still emit a single buffered JSON payload, not a stream.
func TestRunBufferedBodyWithHaltDoesNotStream(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "svc", 100, "only",
		`EMIT @web.res.body = b64"x", @halt = true`)

	resCh := make(chan event.Payload, 16)
	if err := pu.Run(context.Background(), `{}`, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := drain(resCh)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (single buffered payload); msgs=%+v", len(msgs), msgs)
	}
	if msgs[0].Type != event.JSON {
		t.Errorf("msg[0].Type = %v, want JSON (buffered, not streamed)", msgs[0].Type)
	}
	if !gjson.Get(msgs[0].Raw, "_txc.web.res.body").Exists() {
		t.Errorf("buffered payload should still carry _txc.web.res.body; raw=%s", msgs[0].Raw)
	}
}

// TestRunBreakpointPreemptsStreaming locks in the precedence rule: when
// breakpoints are armed (_txc.flag_breakpoint), a non-terminal body write
// must NOT start a stream — the chassis dumps the whole envelope with
// _txc.broke_at as a single JSON payload instead.
func TestRunBreakpointPreemptsStreaming(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "svc", 100, "first",
		`EMIT @web.res.status = 200, @web.res.body = b64"a\n"`)
	seedOp(t, pu, "svc", 200, "second",
		`EMIT @web.res.body = b64"b\n", @halt = true`)

	resCh := make(chan event.Payload, 16)
	in := `{"_txc":{"flag_breakpoint":true,"break":100}}`
	if err := pu.Run(context.Background(), in, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := drain(resCh)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (breakpoint dump); msgs=%+v", len(msgs), msgs)
	}
	if msgs[0].Type != event.JSON {
		t.Errorf("msg[0].Type = %v, want JSON (breakpoint suppresses streaming)", msgs[0].Type)
	}
	if got := gjson.Get(msgs[0].Raw, "_txc.broke_at").Int(); got != 100 {
		t.Errorf("_txc.broke_at = %d, want 100", got)
	}
	for _, m := range msgs {
		if m.Type == event.StreamHead || m.Type == event.StreamChunk {
			t.Errorf("unexpected stream message under breakpoint: %v", m.Type)
		}
	}
}
