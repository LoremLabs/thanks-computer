package imap

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pires/go-proxyproto"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The login line has to distinguish two situations a bare `tls` bool
// collapses: a client on IMAPS whose TLS a front proxy terminated (safe,
// tls=false proxied=true) and a client that really did send credentials in
// the clear (tls=false proxied=false). These cover both halves — the
// listener/proxied fields reaching the log, and the proxy detection that
// feeds them.

func TestLoginLineNamesTheListener(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	h := newHarnessWithLogger(t, config.Config{}, zap.New(core))
	h.account(t, "acme", "paris@example.com", "secret-1", "")

	c := dial(t, h.addr)
	if err := c.Login("paris@example.com", "secret-1").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	entries := logs.FilterMessage("imap login").All()
	if len(entries) != 1 {
		t.Fatalf("imap login lines = %d, want 1", len(entries))
	}
	got := entries[0].ContextMap()
	// The harness listens on 127.0.0.1:0 with no front proxy, so this is
	// the plaintext-and-nobody-in-front shape: the alarming one, and the
	// only one that should ever read this way.
	for field, want := range map[string]any{
		"outcome":  "ok",
		"user":     "paris@example.com",
		"tls":      false,
		"proxied":  false,
		"listener": h.addr,
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
}

func TestFrontedByProxy(t *testing.T) {
	// A PROXY v1 header from the front proxy, as the edge sends it.
	const header = "PROXY TCP4 176.151.108.50 10.0.0.1 51000 1143\r\n"

	// proxied(t) hands back a proxyproto.Conn whose peer has written the
	// header. net.Pipe is unbuffered, so the write has to be concurrent
	// with the read ProxyHeader() performs.
	proxied := func(t *testing.T) *proxyproto.Conn {
		t.Helper()
		srv, cli := net.Pipe()
		t.Cleanup(func() { _ = srv.Close(); _ = cli.Close() })
		go func() {
			_ = cli.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, _ = cli.Write([]byte(header))
		}()
		return proxyproto.NewConn(srv)
	}

	t.Run("bare connection", func(t *testing.T) {
		srv, cli := net.Pipe()
		t.Cleanup(func() { _ = srv.Close(); _ = cli.Close() })
		if frontedByProxy(srv) {
			t.Error("a connection nobody fronted reported as proxied")
		}
	})

	t.Run("plaintext listener behind the proxy", func(t *testing.T) {
		pc := proxied(t)
		if !frontedByProxy(pc) {
			t.Fatal("PROXY header present but not reported")
		}
		// The same header is what makes the logged ip the client's.
		host, _, err := net.SplitHostPort(pc.RemoteAddr().String())
		if err != nil || host != "176.151.108.50" {
			t.Errorf("remote = %v err=%v, want the header's source", pc.RemoteAddr(), err)
		}
	})

	t.Run("IMAPS listener behind the proxy", func(t *testing.T) {
		// tls.NewListener wraps the proxyproto listener, so the session
		// sees a *tls.Conn and the header is one layer down. No handshake
		// happens here: only NetConn() is touched.
		tc := tls.Client(proxied(t), &tls.Config{InsecureSkipVerify: true})
		if !frontedByProxy(tc) {
			t.Error("PROXY header under TLS not reported")
		}
	})
}
