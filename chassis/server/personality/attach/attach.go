// Package attach is the chassis head a machine dials with `txco attach`
// (chassis/attached): three public HTTP routes on a listener of its own.
//
//	POST /v1/attach/code     what a one-use code is for, without spending it
//	POST /v1/attach/enroll   spend the code and enroll the key that signed
//	GET  /v1/attach          the link, a WebSocket carrying yamux streams
//
// OFF by default. Both gates must be flipped:
//   - `attach` must appear in `--personalities`
//   - `--attach-listen-addrs` must be non-empty
//
// The routes authenticate their own callers (a code, then a key); the
// listener is meant to be reached from the internet, behind the same edge
// as every other host.
package attach

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/edgeproxy"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// Controller owns the attach listeners.
type Controller struct {
	ctx context.Context
	pu  *processor.Unit
	svc *attached.Service

	mu        sync.Mutex
	servers   []*http.Server
	listeners []net.Listener
}

// NewController constructs (but does not start) the head. svc is nil on a
// chassis that serves no attached workspaces; the head then stays off.
func NewController(ctx context.Context, pu *processor.Unit, svc *attached.Service) *Controller {
	return &Controller{ctx: ctx, pu: pu, svc: svc}
}

// Addrs returns the addresses the listeners are bound to (":0" resolved).
func (c *Controller) Addrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.listeners))
	for _, ln := range c.listeners {
		out = append(out, ln.Addr().String())
	}
	return out
}

func on(personalities, name string) bool {
	for _, p := range strings.Split(personalities, ",") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	return false
}

// Start binds the listeners and serves.
func (c *Controller) Start() {
	if !on(c.pu.Conf.Personalities, "attach") {
		return
	}
	var addrs []string
	for _, a := range c.pu.Conf.AttachListenAddrs {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		c.pu.Logger.Info("attach personality enabled but no listen addrs; head not started")
		return
	}
	if c.svc == nil {
		c.pu.Logger.Warn("attach personality enabled but this chassis serves no attached workspaces (needs a workspace provider and a runtime store); head not started")
		return
	}
	c.svc.Hub.Logf = func(format string, args ...any) { c.pu.Logger.Sugar().Infof(format, args...) }

	h := attached.NewHTTP(c.svc, 0)
	clients, _ := edgeproxy.NewClients(c.pu.Conf.WebTrustedProxies)
	h.ClientIP = clients.IP
	mux := http.NewServeMux()
	h.Register(mux)

	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			c.pu.Logger.Fatal("attach socket already in use (or otherwise unbindable)",
				zap.String("addr", addr), zap.String("err", err.Error()))
		}
		srv := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return c.ctx },
		}
		c.mu.Lock()
		c.listeners = append(c.listeners, ln)
		c.servers = append(c.servers, srv)
		c.mu.Unlock()
		c.pu.Logger.Info("attach controller started", zap.String("addr", ln.Addr().String()))
		go func() { _ = srv.Serve(ln) }()
	}
}

// Stop closes the listeners and every link. A machine told `shutdown` dials
// again, to whichever node answers.
func (c *Controller) Stop() {
	c.mu.Lock()
	servers := c.servers
	c.servers, c.listeners = nil, nil
	c.mu.Unlock()
	for _, s := range servers {
		_ = s.Close()
	}
	if c.svc != nil {
		c.svc.Hub.CloseAll(attached.ReasonShutdown)
	}
}
