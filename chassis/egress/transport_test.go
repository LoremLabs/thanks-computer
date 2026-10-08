package egress_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
)

// No environment proxy: through one, the guard would check the proxy's
// address, not the destination's.
func TestTransportNeverUsesAProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	g, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if egress.Transport(g).Proxy != nil {
		t.Fatal("Transport keeps a Proxy func; it must be nil")
	}
	// The stdlib default it is cloned from still has one, so a client
	// built from it by hand would not be covered.
	if http.DefaultTransport.(*http.Transport).Proxy == nil {
		t.Fatal("http.DefaultTransport has no Proxy: this test no longer proves anything")
	}
}

func TestTransportDialsThroughTheGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	for policy, wantBlocked := range map[string]bool{"private": true, "open": false} {
		g, err := egress.Open(policy, egress.Config{})
		if err != nil {
			t.Fatal(err)
		}
		c := &http.Client{Transport: egress.Transport(g), Timeout: 2 * time.Second}
		resp, err := c.Get(srv.URL)
		if resp != nil {
			resp.Body.Close()
		}
		if wantBlocked && (err == nil || !strings.Contains(err.Error(), "egress: blocked")) {
			t.Fatalf("%s: err = %v, want the guard's refusal of loopback", policy, err)
		}
		if !wantBlocked && err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
	}
}
