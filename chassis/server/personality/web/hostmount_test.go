package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestMountHostClaimsOnlyItsHosts drives the real router: a host mount takes
// EVERY path on the hostnames it matches and nothing on any other — so a
// tenant's own `/p/…` route on its ordinary hostname still reaches its stack.
func TestMountHostClaimsOnlyItsHosts(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "web", WebAddr: "127.0.0.1:0", OpTimeoutMax: "5s",
			WebWriteTimeout: 15, WebReadTimeout: 15, WebIdleTimeout: 60, WebMaxBodyBytes: 1 << 20,
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	web := NewController(ctx, pu, nil)
	web.MountHost(
		func(host string) bool { return strings.HasPrefix(host, "ipp.") },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "mount:"+r.URL.Path)
		}))
	go func() {
		for env := range bus { // the "stack": every other request lands here
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

	get := func(host, path string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s%s: %v", host, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	for _, path := range []string{"/p/research", "/", "/drive/x", "/anything/else"} {
		if got := get("ipp.example.com", path); got != "mount:"+path {
			t.Errorf("ipp host %s: got %q, want the mount", path, got)
		}
	}
	if got := get("ipp.example.com:8443", "/p/research"); got != "mount:/p/research" {
		t.Errorf("ipp host with a port: got %q", got)
	}
	// The same path on an ordinary hostname is the tenant's.
	for _, host := range []string{"pony.example.com", "notipp.example.com", "example.com"} {
		if got := get(host, "/p/research"); !strings.Contains(got, "stack") {
			t.Errorf("%s/p/research: got %q, want the stack", host, got)
		}
	}
}
