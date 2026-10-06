package web

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestWriteResHeaders: every distinct value is its own header line, for
// every header, whatever case the envelope spelled the key in; an exact
// repeat is written once; the envelope's values replace any already set.
func TestWriteResHeaders(t *testing.T) {
	for name, c := range map[string]struct {
		header, headers string
		want            []string
	}{
		"two cookies":             {"Set-Cookie", `{"set-cookie":["a=1","b=2"]}`, []string{"a=1", "b=2"}},
		"an exact repeat is once": {"Set-Cookie", `{"set-cookie":["a=1","b=2","a=1"]}`, []string{"a=1", "b=2"}},
		"both spellings merge":    {"Set-Cookie", `{"set-cookie":["a=1"],"Set-Cookie":["b=2","a=1"]}`, []string{"a=1", "b=2"}},
		"a scalar":                {"Set-Cookie", `{"set-cookie":"a=1"}`, []string{"a=1"}},
		"an empty cookie is none": {"Set-Cookie", `{"set-cookie":["","a=1"]}`, []string{"a=1"}},
		"a list-valued header":    {"Vary", `{"vary":["Accept","Cookie","Accept"]}`, []string{"Accept", "Cookie"}},
		"a link per value":        {"Link", `{"link":["</a.css>; rel=preload","</b.js>; rel=preload"]}`, []string{"</a.css>; rel=preload", "</b.js>; rel=preload"}},
		"an empty value is sent":  {"X-Empty", `{"x-empty":[""]}`, []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			h := http.Header{}
			h.Set(c.header, "stale") // the envelope's values replace it
			if conflicts := writeResHeaders(h, `{"_txc":{"web":{"res":{"headers":`+c.headers+`}}}}`); conflicts != nil {
				t.Errorf("conflicts = %q, want none", conflicts)
			}
			if got := h.Values(c.header); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%s = %q, want %q", c.header, got, c.want)
			}
		})
	}
}

// TestWriteResHeadersConflicts: two different values for a single-value
// field are both sent (the chassis doesn't pick for the app) and reported,
// in envelope order; a repeat of the same value is not a conflict.
func TestWriteResHeadersConflicts(t *testing.T) {
	h := http.Header{}
	conflicts := writeResHeaders(h, `{"_txc":{"web":{"res":{"headers":{`+
		`"location":["/a","/b"],"content-type":["text/html","text/plain"],`+
		`"etag":["\"x\"","\"x\""],"vary":["Accept","Cookie"]}}}}}`)
	if want := []string{"Location", "Content-Type"}; !reflect.DeepEqual(conflicts, want) {
		t.Errorf("conflicts = %q, want %q", conflicts, want)
	}
	if got := h.Values("Content-Type"); !reflect.DeepEqual(got, []string{"text/html", "text/plain"}) {
		t.Errorf("Content-Type = %q, want both values", got)
	}
	if got := h.Values("Location"); !reflect.DeepEqual(got, []string{"/a", "/b"}) {
		t.Errorf("Location = %q, want both values", got)
	}
	if got := h.Values("Etag"); !reflect.DeepEqual(got, []string{`"x"`}) {
		t.Errorf("ETag = %q, want one", got)
	}
}

// TestBufferedResponseWritesEveryCookie runs a real request through the
// buffered writer: the shape two ops' merged outputs take (see the
// processor's TestRunResponseHeadersAppendAcrossScopes) arrives as every
// value the ops wrote, and the stack's second Content-Type is logged as the
// bug it is.
func TestBufferedResponseWritesEveryCookie(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "web", WebAddr: "127.0.0.1:0", OpTimeoutMax: "5s",
			WebWriteTimeout: 15, WebReadTimeout: 15, WebIdleTimeout: 60, WebMaxBodyBytes: 1 << 20,
		},
		Logger: zap.New(core),
		Bus:    bus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	web := NewController(ctx, pu, nil)
	go func() {
		for env := range bus {
			env.ResCh <- event.Payload{Type: event.JSON, Raw: `{"_txc":{"web":{"res":{"status":200,` +
				`"headers":{"content-type":["text/html","text/plain"],"set-cookie":["dxid=1; Path=/","dcoh=2; Path=/"]},` +
				`"body":"eA=="}}}}`}
		}
	}()
	web.Start()
	var addr string
	select {
	case addr = <-web.bound:
	case <-time.After(5 * time.Second):
		t.Fatal("web head did not bind")
	}
	t.Cleanup(func() { web.Stop(); cancel() })

	resp, err := http.Get("http://" + addr + "/events/in")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Values("Content-Type"); !reflect.DeepEqual(got, []string{"text/html", "text/plain"}) {
		t.Errorf("Content-Type = %q, want both values the stack wrote", got)
	}
	if got := resp.Header.Values("Set-Cookie"); !reflect.DeepEqual(got, []string{"dxid=1; Path=/", "dcoh=2; Path=/"}) {
		t.Errorf("Set-Cookie = %q, want both cookies", got)
	}
	warned := logs.FilterMessageSnippet("single-value header").All()
	if len(warned) != 1 {
		t.Fatalf("want one conflict warning, got %d", len(warned))
	}
	if got := warned[0].ContextMap()["headers"]; !reflect.DeepEqual(got, []interface{}{"Content-Type"}) {
		t.Errorf("warning names %v, want [Content-Type]", got)
	}
}
