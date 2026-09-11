package processor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// --- the sink itself ------------------------------------------------------

// collect drains ch until it sees a StreamEnd (or the deadline), returning
// the payloads in order.
func collect(t *testing.T, ch chan event.Payload, want int) []event.Payload {
	t.Helper()
	var got []event.Payload
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case p := <-ch:
			got = append(got, p)
			if p.Type == event.StreamEnd {
				return got
			}
		case <-deadline:
			t.Fatalf("timed out after %d payloads: %+v", len(got), got)
		}
	}
	return got
}

func TestStreamSinkHeadThenChunks(t *testing.T) {
	ch := make(chan event.Payload)
	s := &streamSink{resCh: ch}
	ctx := context.Background()

	var got []event.Payload
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); got = collect(t, ch, 4) }()

	if s.Opened() {
		t.Error("a fresh sink reports opened")
	}
	// An empty write must not open the stream: a command that produces
	// nothing has to leave the normal JSON response intact.
	if err := s.Write(ctx, `{"head":true}`, nil); err != nil {
		t.Fatal(err)
	}
	if s.Opened() {
		t.Fatal("an empty write opened the stream")
	}
	if err := s.Write(ctx, `{"head":true}`, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if !s.Opened() {
		t.Fatal("a write did not open the stream")
	}
	if err := s.Write(ctx, `{"head":"ignored"}`, []byte("two")); err != nil {
		t.Fatal(err)
	}
	ch <- event.Payload{Type: event.StreamEnd}
	wg.Wait()

	if len(got) != 4 {
		t.Fatalf("payloads = %d, want 4 (head, two chunks, end): %+v", len(got), got)
	}
	if got[0].Type != event.StreamHead || got[0].Raw != `{"head":true}` {
		t.Errorf("first payload = %+v, want the head", got[0])
	}
	// The head is sent once: the second write's head argument is ignored.
	if got[1].Type != event.StreamChunk || got[1].Raw != "one" {
		t.Errorf("second payload = %+v", got[1])
	}
	if got[2].Type != event.StreamChunk || got[2].Raw != "two" {
		t.Errorf("third payload = %+v", got[2])
	}
	if got[3].Type != event.StreamEnd {
		t.Errorf("fourth payload = %+v", got[3])
	}
	if s.Bytes() != 6 {
		t.Errorf("bytes = %d, want 6", s.Bytes())
	}
}

func TestStreamSinkClosedAndCancelled(t *testing.T) {
	// After markClosed a write is a silent no-op — a late write from an op
	// goroutine must not race bytes onto a finished response.
	s := &streamSink{resCh: make(chan event.Payload)}
	s.markClosed()
	if err := s.Write(context.Background(), "{}", []byte("late")); err != nil {
		t.Errorf("write after close = %v, want nil", err)
	}
	if s.Opened() {
		t.Error("a write after close opened the stream")
	}

	// A client that goes away cancels the op ctx; the blocked writer must
	// unblock with that error rather than wedging the command's copy loop.
	s2 := &streamSink{resCh: make(chan event.Payload)} // nobody receiving
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s2.Write(ctx, "{}", []byte("x")) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("write on a cancelled ctx = nil, want the ctx error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write did not unblock on ctx cancel")
	}
}

func TestWorkspaceStreamHead(t *testing.T) {
	// Default: 200 + a plain-text content type (stdout is bytes, not JSON).
	h := workspaceStreamHead(`{}`)
	if gjson.Get(h, "_txc.web.res.status").Int() != 200 {
		t.Errorf("default status: %s", h)
	}
	if !strings.HasPrefix(gjson.Get(h, "_txc.web.res.headers.content-type.0").String(), "text/plain") {
		t.Errorf("default content-type: %s", h)
	}
	// An author's own status and headers survive; a staged body does not
	// (the body of a streamed response is the chunks).
	in := `{"_txc":{"web":{"res":{"status":201,"headers":{"content-type":["text/csv"],"x-thing":["1"]},"body":"YQ=="}}}}`
	h = workspaceStreamHead(in)
	if gjson.Get(h, "_txc.web.res.status").Int() != 201 ||
		gjson.Get(h, "_txc.web.res.headers.content-type.0").String() != "text/csv" ||
		gjson.Get(h, "_txc.web.res.headers.x-thing.0").String() != "1" {
		t.Errorf("author head not preserved: %s", h)
	}
	if gjson.Get(h, "_txc.web.res.body").Exists() {
		t.Errorf("staged body leaked into the head: %s", h)
	}
}

// --- ExecWorkspace: the opt-in and its refusals ---------------------------

func TestExecWorkspaceStreamRefusals(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	// A sink is present for the cases that are refused for other reasons.
	sctx := withStreamSink(ctx, &streamSink{resCh: make(chan event.Payload, 4)})

	cases := []struct {
		name, meta, want string
		loop             bool
		ctx              context.Context
	}{
		{name: "with LOOP", meta: `{"command":"x","stream":true}`, want: "LOOP", loop: true, ctx: sctx},
		{name: "with mode", meta: `{"command":"x","stream":true,"mode":"continuable"}`, want: "mode", ctx: sctx},
		{name: "with secrets", meta: `{"command":"x","stream":true,"secrets":{"env":{"T":{"secret":"S"}}}}`, want: "secrets", ctx: sctx},
		{name: "no sink", meta: `{"command":"x","stream":true}`, want: "live HTTP request", ctx: ctx},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := workspaceOp("workspace://tools/exec", c.meta)
			if c.loop {
				op.Resonator.Loop = &resonator.Loop{Max: 2}
			}
			p, err := pu.ExecWorkspace(c.ctx, op)
			if err != nil {
				t.Fatalf("dropped (%v), want an in-band refusal", err)
			}
			if gjson.Get(p.Raw, "workspace.error.code").String() != "bad_request" {
				t.Fatalf("payload = %s", p.Raw)
			}
			if msg := gjson.Get(p.Raw, "workspace.error.message").String(); !strings.Contains(msg, c.want) {
				t.Errorf("message = %q, want it to mention %q", msg, c.want)
			}
			if stub.execs.Load() != 0 {
				t.Errorf("a refused stream still dispatched (%d execs)", stub.execs.Load())
			}
		})
	}
}

func TestExecWorkspaceStreamResultShape(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}, stream: []string{"alpha\n", "beta\n"}}
	pu, ctx := newWorkspaceUnit(t, stub)
	ch := make(chan event.Payload, 8) // buffered: this test is not about backpressure
	sctx := withStreamSink(ctx, &streamSink{resCh: ch})

	p, err := pu.ExecWorkspace(sctx, workspaceOp("workspace://tools/exec", `{"command":"build","stream":true,"into":"_ws"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The bytes went to the client; the envelope says so and carries the
	// count instead of the payload.
	if !gjson.Get(p.Raw, "_ws.stdout_streamed").Bool() {
		t.Errorf("stdout_streamed missing: %s", p.Raw)
	}
	if got := gjson.Get(p.Raw, "_ws.stdout_bytes").Int(); got != 11 {
		t.Errorf("stdout_bytes = %d, want 11: %s", got, p.Raw)
	}
	if gjson.Get(p.Raw, "_ws.stdout").Exists() {
		t.Errorf("streamed stdout also rode the envelope: %s", p.Raw)
	}
	if gjson.Get(p.Raw, "_ws.exit").Int() != 0 || !gjson.Get(p.Raw, "_ws.stderr").Exists() {
		t.Errorf("exit/stderr missing: %s", p.Raw)
	}

	got := []event.Payload{}
	for len(ch) > 0 {
		got = append(got, <-ch)
	}
	if len(got) != 3 || got[0].Type != event.StreamHead || got[1].Raw != "alpha\n" || got[2].Raw != "beta\n" {
		t.Fatalf("stream payloads = %+v", got)
	}
}

// --- end to end through Run ----------------------------------------------

// TestWorkspaceStreamEndToEnd is the proof that a command's output reaches
// the client as it is produced: Run emits StreamHead, a chunk per provider
// write, and a terminating StreamEnd — and NO buffered JSON payload, because
// the head has already gone out.
func TestWorkspaceStreamEndToEnd(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}, stream: []string{"step 1\n", "step 2\n", "done\n"}}
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.WorkspaceDefaultTimeout = "60s"
	seedWorkspaceOp(t, pu, "site", 100, "build",
		`EXEC "workspace://tools/exec" WITH command = "make", stream = true, into = "_ws"`)

	resCh := make(chan event.Payload) // unbuffered, like the web outlet's
	done := make(chan error, 1)
	go func() {
		done <- pu.Run(context.Background(), `{"_txc":{"tenant":"acme","src":"http"}}`, "site/100", resCh)
	}()

	var got []event.Payload
	for {
		select {
		case p := <-resCh:
			got = append(got, p)
			if p.Type == event.StreamEnd || p.Type == event.JSON {
				goto collected
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no terminal payload; got %+v", got)
		}
	}
collected:
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}

	if len(got) != 5 {
		t.Fatalf("payloads = %d, want 5 (head + 3 chunks + end): %+v", len(got), got)
	}
	if got[0].Type != event.StreamHead {
		t.Fatalf("first payload = %v, want StreamHead", got[0].Type)
	}
	if gjson.Get(got[0].Raw, "_txc.web.res.status").Int() != 200 {
		t.Errorf("head status: %s", got[0].Raw)
	}
	for i, want := range []string{"step 1\n", "step 2\n", "done\n"} {
		p := got[i+1]
		if p.Type != event.StreamChunk || p.Raw != want {
			t.Errorf("chunk %d = %+v, want %q", i, p, want)
		}
	}
	if got[4].Type != event.StreamEnd {
		t.Errorf("last payload = %v, want StreamEnd", got[4].Type)
	}
	// The terminator carries the final envelope so the request still
	// accounts for itself: without it the dispatch tee captures nothing and
	// the usage line records no tenant, no fuel and no bytes.
	if got[4].Raw == "" {
		t.Fatal("StreamEnd carries no envelope: a streamed request would account for nothing")
	}
	if got[4].Raw == "" || gjson.Get(got[4].Raw, "_txc.tenant").String() != "acme" {
		t.Errorf("StreamEnd envelope lost the tenant: %s", got[4].Raw)
	}
	if FuelUsedFromEnvelope(got[4].Raw) <= 0 {
		t.Errorf("StreamEnd envelope carries no fuel: %s", got[4].Raw)
	}
	if gjson.Get(got[4].Raw, "_ws.exit").Int() != 0 {
		t.Errorf("StreamEnd envelope is not the merged result: %s", got[4].Raw)
	}
}

// TestWorkspaceStreamSilentCommandKeepsJSON: a streamed exec that produces
// no output never opens the stream, so the request still renders its normal
// JSON response.
func TestWorkspaceStreamSilentCommandKeepsJSON(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 7}} // no stream writes
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.WorkspaceDefaultTimeout = "60s"
	seedWorkspaceOp(t, pu, "site", 100, "quiet",
		`EXEC "workspace://tools/exec" WITH command = "true", stream = true, into = "_ws"`)

	resCh := make(chan event.Payload, 4)
	done := make(chan error, 1)
	go func() {
		done <- pu.Run(context.Background(), `{"_txc":{"tenant":"acme","src":"http"}}`, "site/100", resCh)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	p := <-resCh
	if p.Type != event.JSON {
		t.Fatalf("payload type = %v, want JSON", p.Type)
	}
	if gjson.Get(p.Raw, "_ws.exit").Int() != 7 || !gjson.Get(p.Raw, "_ws.stdout_streamed").Bool() {
		t.Errorf("payload = %s", p.Raw)
	}
}
