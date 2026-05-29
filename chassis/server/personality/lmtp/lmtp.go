// Package lmtp implements the chassis's LMTP head: a personality that
// speaks LMTP (RFC 2033) to a colocated Postfix (or any LMTP client)
// and turns each delivery into a normal txcl envelope.
//
// Phase 0 scope: protocol skeleton only. Raw RFC 5322 bytes ride the
// envelope as `_txc.lmtp.msg.raw` (b64); MIME parsing lands in Phase 1.
// The verdict is broadcast (same status for every recipient) using the
// plain `smtp.Session` interface; per-recipient verdicts via
// `smtp.LMTPSession` are Phase 3.
//
// Default-deny: if the pipeline doesn't return an explicit
// `_txc.lmtp.res.code`, every recipient gets 550 5.1.1. Mail-convention
// "user unknown" is the safer default than silently accepting (250) or
// queue-forever-then-bounce (4xx).
//
// LMTP is OFF by default. Both gates must be flipped:
//   - `lmtp` must appear in `--personalities`
//   - `--lmtp-listen-addrs` must be non-empty
package lmtp

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// LMTPController owns the LMTP listeners and their go-smtp Servers.
//
// One Controller can host multiple listeners (e.g. a Unix socket for
// Postfix plus a TCP socket for cross-host LMTP); each listener gets
// its own `smtp.Server`. The Controller doesn't care which listener a
// delivery arrived on — that string flows into the envelope as
// `_txc.lmtp.listener` for ingress routing to inspect.
type LMTPController struct {
	ctx       context.Context
	pu        *processor.Unit
	resolver  ingress.MailResolver // may be nil; every rcpt then unrouted → 550
	servers   []*smtp.Server
	listeners []net.Listener
	shutdown  chan bool
	wg        sync.WaitGroup
}

// boundAddrs returns the actual network addresses each listener is
// bound to. Exposed for tests that bind to an ephemeral port (":0") and
// need to recover the assigned port before dialing.
func (l *LMTPController) boundAddrs() []string {
	addrs := make([]string, 0, len(l.listeners))
	for _, ln := range l.listeners {
		if ln != nil {
			addrs = append(addrs, ln.Addr().String())
		}
	}
	return addrs
}

// NewController constructs (but does not start) an LMTP controller.
// Mirrors the constructor shape of the other personalities so server.go
// can treat them uniformly.
//
// `resolver` is the chassis's ingress resolver, restricted to its
// MailResolver facet. The chassis-side wiring in server.go type-asserts
// the data-plane resolver against MailResolver and passes it through;
// pass nil for embedders / tests that don't need ingress routing — every
// RCPT TO then default-denies (550) for lack of any opted-in stack.
func NewController(ctx context.Context, pu *processor.Unit, resolver ingress.MailResolver) *LMTPController {
	return &LMTPController{
		ctx:      ctx,
		pu:       pu,
		resolver: resolver,
		shutdown: make(chan bool),
	}
}

// Start binds the configured listeners and serves LMTP on each. The
// double-gate (personality string AND non-empty listen addrs) means
// existing deployments cannot acquire a new listener on upgrade
// without explicit opt-in.
func (l *LMTPController) Start() {
	if !strings.Contains(l.pu.Conf.Personalities, "lmtp") {
		return
	}

	addrs := nonEmpty(l.pu.Conf.LMTPListenAddrs)
	if len(addrs) == 0 {
		// Personality enabled but no listen addresses configured —
		// not an error. An operator who wants LMTP off in this env
		// can leave --lmtp-listen-addrs empty and the head stays
		// silent. Log so misconfiguration is visible.
		l.pu.Logger.Info("lmtp personality enabled but no listen addrs; head not started")
		return
	}

	readTimeout, err := time.ParseDuration(l.pu.Conf.LMTPReadTimeout)
	if err != nil {
		l.pu.Logger.Warn("invalid lmtp-read-timeout, using 30s",
			zap.String("value", l.pu.Conf.LMTPReadTimeout),
			zap.String("err", err.Error()))
		readTimeout = 30 * time.Second
	}
	dataTimeout, err := time.ParseDuration(l.pu.Conf.LMTPDataTimeout)
	if err != nil {
		l.pu.Logger.Warn("invalid lmtp-data-timeout, using 60s",
			zap.String("value", l.pu.Conf.LMTPDataTimeout),
			zap.String("err", err.Error()))
		dataTimeout = 60 * time.Second
	}

	hostname := l.pu.Conf.LMTPHostname
	if hostname == "" {
		if h, err := os.Hostname(); err == nil {
			hostname = h
		} else {
			hostname = "localhost"
		}
	}

	backend := &lmtpBackend{ctrl: l}

	for _, addr := range addrs {
		network, bind := parseListenAddr(addr)

		// Pre-bind BEFORE logging "lmtp controller started" so a
		// socket conflict surfaces with a clear, actionable error
		// before the operator sees anything resembling "ready".
		// Matches tcp.go's pre-bind discipline.
		if network == "unix" {
			// Best-effort cleanup of a stale socket left behind by a
			// crashed previous instance. We only unlink paths that
			// look like a socket (Mode().Type() == ModeSocket) — never
			// a regular file. If the unlink fails, fall through and
			// let net.Listen report the bind error.
			if fi, statErr := os.Stat(bind); statErr == nil && fi.Mode()&os.ModeSocket != 0 {
				_ = os.Remove(bind)
			}
		}

		listener, err := net.Listen(network, bind)
		if err != nil {
			hint := "lsof -iTCP" + bind + " -sTCP:LISTEN"
			if network == "unix" {
				hint = "lsof -U | grep " + bind
			}
			l.pu.Logger.Fatal("lmtp socket already in use (or otherwise unbindable)",
				zap.String("addr", addr),
				zap.String("network", network),
				zap.String("bind", bind),
				zap.String("err", err.Error()),
				zap.String("hint", hint))
		}

		s := smtp.NewServer(backend)
		s.LMTP = true
		s.Domain = hostname
		s.Network = network
		s.Addr = bind
		s.MaxMessageBytes = int64(l.pu.Conf.LMTPMaxMsgBytes)
		s.MaxRecipients = l.pu.Conf.LMTPMaxRecipients
		s.ReadTimeout = readTimeout
		s.WriteTimeout = dataTimeout
		s.ErrorLog = &zapErrorLog{logger: l.pu.Logger}

		l.servers = append(l.servers, s)
		l.listeners = append(l.listeners, listener)
		l.pu.Logger.Info("lmtp controller started",
			zap.String("addr", addr),
			zap.String("network", network),
			zap.String("bind", bind))

		l.wg.Add(1)
		go func(srv *smtp.Server, ln net.Listener) {
			defer l.wg.Done()
			if err := srv.Serve(ln); err != nil && err != smtp.ErrServerClosed {
				l.pu.Logger.Error("lmtp serve error",
					zap.String("err", err.Error()))
			}
		}(s, listener)
	}
}

// Stop drains in-flight LMTP transactions and closes the listeners.
// Each go-smtp Server is shut down with a 5s ceiling so a wedged
// session can't stall chassis shutdown.
func (l *LMTPController) Stop() {
	if !strings.Contains(l.pu.Conf.Personalities, "lmtp") {
		return
	}
	if len(l.servers) == 0 {
		return
	}

	l.pu.Logger.Info("calling lmtp controller stop")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, s := range l.servers {
		if err := s.Shutdown(ctx); err != nil {
			l.pu.Logger.Warn("lmtp shutdown error",
				zap.String("err", err.Error()))
		}
	}
	l.wg.Wait()
	l.pu.Logger.Info("lmtp controller stopped")
}

// parseListenAddr splits a config value into (network, address) for
// `net.Listen`. Accepts `unix:/path`, `tcp:host:port`, or a bare
// `:port` / `host:port` (defaulting to tcp).
func parseListenAddr(addr string) (network, bind string) {
	if strings.HasPrefix(addr, "unix:") {
		return "unix", strings.TrimPrefix(addr, "unix:")
	}
	if strings.HasPrefix(addr, "tcp:") {
		return "tcp", strings.TrimPrefix(addr, "tcp:")
	}
	return "tcp", addr
}

// nonEmpty drops blank entries from a slice (viper's []string parsing
// can yield a single "" element when the flag is set explicitly empty;
// be defensive).
func nonEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// zapErrorLog adapts the chassis zap logger to go-smtp's Logger
// interface so the library's internal errors land in the same log
// stream as the rest of the chassis (instead of stdout via the default
// `log.Logger`).
type zapErrorLog struct {
	logger *zap.Logger
}

func (z *zapErrorLog) Printf(format string, v ...interface{}) {
	z.logger.Warn("lmtp lib: " + fmt.Sprintf(format, v...))
}

func (z *zapErrorLog) Println(v ...interface{}) {
	z.logger.Warn("lmtp lib: " + fmt.Sprintln(v...))
}
