package web

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	websocketp "github.com/loremlabs/thanks-computer/chassis/server/personality/websocket"
)

// TestWebSocketHandoff drives the real web head end to end: an upgrade
// request is stamped and runs through the bus; when the "stack" (a fake
// responder standing in for txco://websocket/accept) records an accept
// against the minted session id, the handler upgrades and the session's
// messages make their own runs; without an accept the same request is an
// ordinary HTTP response; a plain GET is never stamped.
func TestWebSocketHandoff(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:           "web,websocket",
			WebAddr:                 "127.0.0.1:0",
			OpTimeoutMax:            "5s",
			WebWriteTimeout:         15,
			WebReadTimeout:          15,
			WebIdleTimeout:          60,
			WebMaxBodyBytes:         1 << 20,
			WebsocketPingInterval:   "200ms",
			WebsocketWriteTimeout:   "1s",
			WebsocketRunTimeout:     "2s",
			WebsocketDrainTimeout:   "2s",
			WebsocketIdleTimeout:    "10s",
			WebsocketMaxIdleTimeout: "1h",
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}
	ctx, cancel := context.WithCancel(context.Background())
	ws := websocketp.NewController(ctx, pu)
	web := NewController(ctx, pu, nil)
	web.SetWebSocket(ws)

	var acceptUpgrades atomic.Bool
	acceptUpgrades.Store(true)
	var plainStamped atomic.Bool
	go func() {
		for env := range bus {
			raw := env.Payload.Raw
			switch gjson.Get(raw, "_txc.src").String() {
			case "http":
				upgrade := gjson.Get(raw, "_txc.websocket.upgrade").Bool()
				if gjson.Get(raw, "_txc.web.req.url.path").String() == "/plain" && gjson.Get(raw, "_txc.websocket").Exists() {
					plainStamped.Store(true)
				}
				if upgrade && acceptUpgrades.Load() {
					sid := gjson.Get(raw, "_txc.websocket.session.id").String()
					_ = ws.RecordAccept(sid, websocketp.Accept{Tenant: "acme", Stack: "counter"})
				}
				env.ResCh <- event.Payload{Raw: `{"hello":"http"}`, Type: event.JSON}
			case "websocket":
				sid := gjson.Get(raw, "_txc.websocket.session.id").String()
				text := gjson.Get(raw, "_txc.websocket.msg.text").String()
				_ = ws.Send(context.Background(), "acme", sid, websocketp.MessageText, []byte("echo:"+text))
				env.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
			default:
				env.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
			}
		}
	}()

	ws.Start()
	web.Start()
	var addr string
	select {
	case addr = <-web.bound:
	case <-time.After(5 * time.Second):
		t.Fatal("web head did not bind")
	}
	t.Cleanup(func() {
		ws.Stop()
		web.Stop()
		cancel()
	})

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()

	// Accepted: 101, and the session echoes through the bus.
	c, resp, err := websocket.Dial(dctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	defer c.CloseNow()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("101 lost the X-Request-Id header")
	}
	if err := c.Write(dctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	_, data, err := c.Read(dctx)
	if err != nil || string(data) != "echo:hi" {
		t.Fatalf("echo = %q err=%v", data, err)
	}
	if ws.Count() != 1 {
		t.Errorf("sessions = %d", ws.Count())
	}

	// Not accepted: the same upgrade request is an ordinary HTTP response.
	acceptUpgrades.Store(false)
	_, resp, err = websocket.Dial(dctx, "ws://"+addr+"/ws", nil)
	if err == nil {
		t.Fatal("dial succeeded without an accept")
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("unaccepted upgrade: resp=%v", resp)
	}

	// A plain GET is untouched by the personality.
	r, err := http.Get("http://" + addr + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(string(body), "hello") {
		t.Fatalf("plain GET: %d %s", r.StatusCode, body)
	}
	if plainStamped.Load() {
		t.Error("a plain GET was stamped with _txc.websocket")
	}
}
