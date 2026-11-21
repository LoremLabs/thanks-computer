// Package attach is the chassis head a machine dials with `txco attach`
// (chassis/attached): three public HTTP routes.
//
//	POST /v1/attach/code     what a one-use code is for, without spending it
//	POST /v1/attach/enroll   spend the code and enroll the key that signed
//	GET  /v1/attach          the link, a WebSocket carrying yamux streams
//
// OFF by default: `attach` must appear in `--personalities`. It is then
// served ONE of two ways:
//
//   - ON THE WEB HEAD, with no port of its own, when --attach-public-url
//     names a host and --attach-listen-addrs is empty. The web head hands
//     every request for exactly that hostname to this head (MountHost, as
//     the ipp head is mounted) and no stack ever sees one. This is how a
//     fleet runs it: one more hostname at the edge, nothing else.
//   - ON A LISTENER OF ITS OWN, when --attach-listen-addrs is set. For a
//     chassis that wants the head apart from its web head, and for `txco
//     dev --attach`, where every stack shares the hostname localhost.
//
// The routes authenticate their own callers (a code, then a key), so the
// head is meant to be reached from the internet, behind the same edge as
// every other host.
package attach

import (
	"context"
	"net"
	"net/http"
	"net/url"
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
	handler   http.Handler
}

// on reports whether the head is wanted at all on this chassis.
func (c *Controller) on() bool {
	return c.svc != nil && on(c.pu.Conf.Personalities, "attach")
}

func (c *Controller) listenAddrs() []string {
	var addrs []string
	for _, a := range c.pu.Conf.AttachListenAddrs {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// MountHost is the hostname the web head should hand to this head, or ""
// when the head is off or has a listener of its own. It is the host of
// --attach-public-url, without a port: the name machines dial.
func (c *Controller) MountHost() string {
	if !c.on() || len(c.listenAddrs()) > 0 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(c.pu.Conf.AttachPublicURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// MatchHost returns the predicate the web head runs on every request's Host
// header: exactly the mounted hostname, whatever port the header names. A
// pure comparison, no lookups.
func MatchHost(mounted string) func(host string) bool {
	return func(host string) bool {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		return mounted != "" && strings.EqualFold(host, mounted)
	}
}

// Handler is the head's three routes, for the web head to mount or for a
// listener of its own to serve.
func (c *Controller) Handler() http.Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handler == nil {
		c.svc.Hub.Logf = func(format string, args ...any) { c.pu.Logger.Sugar().Infof(format, args...) }
		h := attached.NewHTTP(c.svc, 0)
		clients, _ := edgeproxy.NewClients(c.pu.Conf.WebTrustedProxies)
		h.ClientIP = clients.IP
		mux := http.NewServeMux()
		h.Register(mux)
		c.handler = mux
	}
	return c.handler
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

// Start binds the head's own listeners, when it has any. Mounted on the web
// head there is nothing to bind: the web head is already serving it.
func (c *Controller) Start() {
	if !on(c.pu.Conf.Personalities, "attach") {
		return
	}
	if c.svc == nil {
		c.pu.Logger.Warn("attach personality enabled but this chassis serves no attached workspaces (needs a workspace provider and a runtime store); head not started")
		return
	}
	addrs := c.listenAddrs()
	if len(addrs) == 0 {
		switch host := c.MountHost(); {
		case host == "":
			c.pu.Logger.Warn("attach personality enabled but neither --attach-public-url (to mount on the web head) nor --attach-listen-addrs is set; head not started")
		case !on(c.pu.Conf.Personalities, "web"):
			c.pu.Logger.Warn("attach personality is mounted on the web head, and this chassis runs no web head; head not started",
				zap.String("host", host))
		default:
			c.pu.Logger.Info("attach controller started, mounted on the web head", zap.String("host", host))
		}
		return
	}
	handler := c.Handler()
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			c.pu.Logger.Fatal("attach socket already in use (or otherwise unbindable)",
				zap.String("addr", addr), zap.String("err", err.Error()))
		}
		srv := &http.Server{
			Handler:           handler,
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.svc.Shutdown(ctx)
	}
}
