package imap

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The login line has to distinguish two situations a bare `tls` bool
// collapses: a client on IMAPS whose TLS a front proxy terminated (safe,
// tls=false proxied=true) and a client that really did send credentials in
// the clear (tls=false proxied=false). This covers the listener/proxied
// fields reaching the log; the proxy detection that feeds them is
// chassis/edgeproxy's (TestFrontedUnderTLS and friends).

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
