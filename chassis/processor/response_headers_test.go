package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestRunResponseHeadersAppendAcrossScopes pins the shape the web head's
// header writer relies on: header arrays from ops in different scopes
// append, later op last, and the writer sends every value
// (personality/web writeResHeaders).
func TestRunResponseHeadersAppendAcrossScopes(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "svc", 100, "first",
		`EMIT @web.res.headers.vary.0 = "Accept", @web.res.headers.set-cookie.0 = "a=1"`)
	seedOp(t, pu, "svc", 200, "second",
		`EMIT @web.res.headers.vary.0 = "Cookie", @web.res.headers.set-cookie.0 = "b=2", @web.res.body = b64"x", @halt = true`)

	resCh := make(chan event.Payload, 16)
	if err := pu.Run(context.Background(), `{}`, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := drain(resCh)
	if len(msgs) != 1 || msgs[0].Type != event.JSON {
		t.Fatalf("want one buffered payload, got %+v", msgs)
	}
	raw := msgs[0].Raw
	if got := gjson.Get(raw, "_txc.web.res.headers.vary").Raw; got != `["Accept","Cookie"]` {
		t.Errorf("vary = %s, want both, the later op last", got)
	}
	if got := gjson.Get(raw, "_txc.web.res.headers.set-cookie").Raw; got != `["a=1","b=2"]` {
		t.Errorf("set-cookie = %s, want both cookies", got)
	}
}

// TestRunResponseHeaderReplacedByDelete: an op replaces a header an
// earlier op set by deleting it in one scope and setting it in a later one
// (@delete applies after its scope merges, so a delete and a set in the
// same op leave nothing).
func TestRunResponseHeaderReplacedByDelete(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedOp(t, pu, "svc", 100, "first", `EMIT @web.res.headers.content-type.0 = "text/html"`)
	seedOp(t, pu, "svc", 200, "drop", `EMIT @delete = ["@web.res.headers.content-type"]`)
	seedOp(t, pu, "svc", 300, "second",
		`EMIT @web.res.headers.content-type.0 = "text/plain", @web.res.body = b64"x", @halt = true`)

	resCh := make(chan event.Payload, 16)
	if err := pu.Run(context.Background(), `{}`, "svc/100", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := drain(resCh)
	if len(msgs) != 1 {
		t.Fatalf("want one payload, got %+v", msgs)
	}
	if got := gjson.Get(msgs[0].Raw, "_txc.web.res.headers.content-type").Raw; got != `["text/plain"]` {
		t.Errorf("content-type = %s, want only the replacement", got)
	}
}

// TestContinuation202ReplacesHeaderArrays: the chassis's "still running"
// answer owns Location, Content-Type and Cache-Control outright — a value
// the stack left in those arrays must not ride along, now that the web head
// sends every value.
func TestContinuation202ReplacesHeaderArrays(t *testing.T) {
	pu, _ := newTestUnit(t)
	env := func(accept string) string {
		return `{"_txc":{"web":{"req":{"url":{"path":"/go"},"headers":{"Accept":["` + accept + `"]}},` +
			`"res":{"headers":{"location":["/a","/b"],"content-type":["text/html","text/csv"],"cache-control":["public"]}}}}}`
	}
	for name, c := range map[string]struct {
		raw, header, want string
	}{
		"303 location":      {env("text/html"), "location", `["/go?_txc.continuation=rc_1"]`},
		"303 content-type":  {env("text/html"), "content-type", `["text/plain; charset=utf-8"]`},
		"303 cache-control": {env("text/html"), "cache-control", `["no-store"]`},
		"202 location":      {env("application/json"), "location", `["/go?_txc.continuation=rc_1"]`},
		"202 content-type":  {env("application/json"), "content-type", `["application/json"]`},
		"202 retry-after":   {env("application/json"), "retry-after", `["3"]`},
	} {
		t.Run(name, func(t *testing.T) {
			resCh := make(chan event.Payload, 1)
			pu.emitContinuation202(context.Background(), c.raw, "rc_1", resCh)
			out := (<-resCh).Raw
			if got := gjson.Get(out, "_txc.web.res.headers."+c.header).Raw; got != c.want {
				t.Errorf("%s = %s, want %s", c.header, got, c.want)
			}
		})
	}
}
