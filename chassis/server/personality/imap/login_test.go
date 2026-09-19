package imap

import (
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
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
	h.account(t, "acme", "paris@example.com", "bcdf-secret-1", "")

	c := dial(t, h.addr)
	if err := c.Login("paris@example.com", "bcdf-secret-1").Wait(); err != nil {
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

// TestLoginGoesThroughTheIdentityStore — LOGIN verifies a credential: the
// username's binding finds the principal, the id in the password finds the
// credential, and its scopes must name IMAP. The ok line says who signed
// in; the right password for another head is refused like a wrong one.
func TestLoginGoesThroughTheIdentityStore(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	h := newHarnessWithLogger(t, config.Config{}, zap.New(core))
	h.account(t, "acme", "paris@example.com", "bcdf-secret-1", "")
	p, _ := authn.ParsePrincipal("acct:paris@example.com")
	authntest.Grant(t, h.ids, "acme", p, "paris@example.com", "bcdg-calendar-only", "calendar:*:*")

	c := dial(t, h.addr)
	if err := c.Login("paris@example.com", "bcdg-calendar-only").Wait(); err == nil {
		t.Fatal("a calendar-only credential opened IMAP")
	}
	if err := c.Login("paris@example.com", "secret-1").Wait(); err == nil {
		t.Fatal("a password with no credential id signed in")
	}
	if err := c.Login("paris@example.com", "bcdf-secret-1").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	outcomes := map[string]int{}
	var ok map[string]any
	for _, e := range logs.FilterMessage("imap login").All() {
		m := e.ContextMap()
		outcomes[m["outcome"].(string)]++
		if m["outcome"] == "ok" {
			ok = m
		}
	}
	if outcomes["scope"] != 1 || outcomes["failed"] != 1 || outcomes["ok"] != 1 {
		t.Errorf("outcomes = %v, want scope:1 failed:1 ok:1", outcomes)
	}
	if ok["principal"] != p.ID || ok["credential"] == "" {
		t.Errorf("the ok line does not say who signed in: %v", ok)
	}
}
