package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestClientIPIsStampedOnTheEnvelope pins `_txc.client.ip` (`@client.ip`):
// the address a rule may key a rate limit on. X-Forwarded-For is whatever
// the client wrote unless the socket peer is a trusted proxy — and then it
// is the entry the PROXY appended, never the left-most one the client chose.
func TestClientIPIsStampedOnTheEnvelope(t *testing.T) {
	for name, c := range map[string]struct {
		trusted []string
		xff     string
		want    string
	}{
		"no proxy configured: the header is the client's word, ignored": {nil, "203.0.113.9", "127.0.0.1"},
		"behind a trusted proxy: the client it recorded":                {[]string{"127.0.0.0/8"}, "198.51.100.7", "198.51.100.7"},
		"a forged left-most hop does not win":                           {[]string{"127.0.0.0/8"}, "10.9.9.9, 198.51.100.7", "198.51.100.7"},
	} {
		t.Run(name, func(t *testing.T) {
			bus := make(chan *event.Envelope, 1)
			pu := &processor.Unit{
				Conf: config.Config{
					Personalities: "web", WebAddr: "127.0.0.1:0", OpTimeoutMax: "5s", WebTrustedProxies: c.trusted,
					WebWriteTimeout: 15, WebReadTimeout: 15, WebIdleTimeout: 60, WebMaxBodyBytes: 1 << 20,
				},
				Logger: zap.NewNop(),
				Bus:    bus,
			}
			ctx, cancel := context.WithCancel(context.Background())
			web := NewController(ctx, pu, nil)
			seen := make(chan string, 1)
			go func() {
				for env := range bus {
					seen <- env.Payload.Raw
					env.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
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

			req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
			req.Header.Set("X-Forwarded-For", c.xff)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			select {
			case raw := <-seen:
				if got := gjson.Get(raw, "_txc.client.ip").String(); got != c.want {
					t.Fatalf("_txc.client.ip = %q, want %q", got, c.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the request never reached the bus")
			}
		})
	}
}
