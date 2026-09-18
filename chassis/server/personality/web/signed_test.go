package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestSignedRouteAndLogRedaction drives the real router: GET/HEAD
// /_txc/signed/<token> reach the signed handler on an ordinary hostname
// (every other method, and a host a personality claims outright, do not),
// and the access log never records the token — it is a bearer credential.
func TestSignedRouteAndLogRedaction(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	bus := make(chan *event.Envelope, 8)
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
	web.SetSignedHandler(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "signed:"+r.URL.Path)
	})
	web.MountHost(
		func(host string) bool { return strings.HasPrefix(host, "ipp.") },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "mount") }))
	go func() {
		for env := range bus {
			env.ResCh <- event.Payload{Raw: `{"from":"stack"}`, Type: event.JSON}
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

	do := func(method, host, path string) string {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s%s: %v", method, host, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	const secret = "v1.SECRETCLAIMS.SECRETMAC"
	if got := do(http.MethodGet, "pony.example.com", "/_txc/signed/"+secret); got != "signed:/_txc/signed/"+secret {
		t.Errorf("GET: got %q, want the signed handler", got)
	}
	// A write is not a signed read: it is an ordinary request to the stack.
	if got := do(http.MethodPost, "pony.example.com", "/_txc/signed/"+secret); !strings.Contains(got, "stack") {
		t.Errorf("POST: got %q, want the stack", got)
	}
	// A host a personality owns outright stays entirely that head's.
	if got := do(http.MethodGet, "ipp.example.com", "/_txc/signed/"+secret); got != "mount" {
		t.Errorf("claimed host: got %q, want the host mount", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for logs.FilterMessage("web").Len() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var sawRedacted bool
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if strings.Contains(f.String, "SECRET") {
				t.Errorf("log field %q leaked the token: %q", f.Key, f.String)
			}
			if f.Key == "url" && f.String == "/_txc/signed/<redacted>" {
				sawRedacted = true
			}
		}
	}
	if !sawRedacted {
		t.Error("no access-log line recorded the redacted signed path")
	}
}

func TestRedactURI(t *testing.T) {
	for in, want := range map[string]string{
		"/_txc/signed/v1.a.b":      "/_txc/signed/<redacted>",
		"/_txc/signed/v1.a.b?x=1":  "/_txc/signed/<redacted>",
		"/_txc/signed/":            "/_txc/signed/<redacted>",
		"/_txc/signedx":            "/_txc/signedx",
		"/docs/_txc/signed/v1.a.b": "/docs/_txc/signed/v1.a.b",
		"/":                        "/",
	} {
		if got := redactURI(in); got != want {
			t.Errorf("redactURI(%q) = %q, want %q", in, got, want)
		}
	}
}
