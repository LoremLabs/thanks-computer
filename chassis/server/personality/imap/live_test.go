package imap

import (
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// revokeAll revokes every credential the harness's identity store holds for
// principal, as a Rotate or a "remove this device" does.
func (h *harness) revokeAll(t *testing.T, principal string) {
	t.Helper()
	if _, err := h.ids.DB.Exec(`UPDATE credentials SET revoked_at = '2026-09-19T00:00:00Z' WHERE principal_id = ?`, principal); err != nil {
		t.Fatal(err)
	}
}

// LOGIN is the only time a mail client presents its password, and it holds
// its connections for hours. A revoked password must close the sessions it
// already opened — on the next command, on NOOP, and inside a quiet IDLE in
// BOTH of its branches (a mailbox selected, and none).
func TestRevokedCredentialEndsOpenSessions(t *testing.T) {
	const user, principal = "paris@example.com", "acct:paris@example.com"
	short := func(h *harness) { h.ctrl.liveInterval.Store(int64(40 * time.Millisecond)) }
	wait := func() { time.Sleep(90 * time.Millisecond) }

	t.Run("the next command", func(t *testing.T) {
		h := newHarness(t, config.Config{})
		h.account(t, "acme", user, "bcdf-pw", "")
		short(h)
		c := dial(t, h.addr)
		if err := c.Login(user, "bcdf-pw").Wait(); err != nil {
			t.Fatal(err)
		}
		if _, err := c.List("", "*", nil).Collect(); err != nil {
			t.Fatalf("a live session was refused: %v", err)
		}
		h.revokeAll(t, principal)
		wait()
		if _, err := c.List("", "*", nil).Collect(); err == nil {
			t.Fatal("LIST after the credential was revoked should fail (BYE)")
		}
		if err := dial(t, h.addr).Login(user, "bcdf-pw").Wait(); err == nil {
			t.Fatal("a revoked password signed in again")
		}
	})

	t.Run("NOOP", func(t *testing.T) {
		h := newHarness(t, config.Config{})
		h.account(t, "acme", user, "bcdf-pw", "")
		short(h)
		c, _ := selectINBOX(t, h)
		h.revokeAll(t, principal)
		wait()
		if err := c.Noop().Wait(); err == nil {
			t.Fatal("NOOP after the credential was revoked should fail (BYE)")
		}
	})

	for name, selected := range map[string]bool{"IDLE with a mailbox selected": true, "IDLE with none": false} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, config.Config{})
			h.account(t, "acme", user, "bcdf-pw", "")
			short(h)
			c := dial(t, h.addr)
			if selected {
				c, _ = selectINBOX(t, h)
			} else if err := c.Login(user, "bcdf-pw").Wait(); err != nil {
				t.Fatal(err)
			}
			idle, err := c.Idle()
			if err != nil {
				t.Fatal(err)
			}
			h.revokeAll(t, principal)
			done := make(chan error, 1)
			go func() { done <- idle.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("IDLE ended without an error after the credential was revoked")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("a quiet IDLE outlived its revoked credential")
			}
		})
	}

	// "Could not tell" keeps the session: a database blip must not drop
	// every mail client at once.
	t.Run("a store error keeps the session", func(t *testing.T) {
		h := newHarness(t, config.Config{})
		h.account(t, "acme", user, "bcdf-pw", "")
		short(h)
		c := dial(t, h.addr)
		if err := c.Login(user, "bcdf-pw").Wait(); err != nil {
			t.Fatal(err)
		}
		_ = h.ids.DB.Close()
		wait()
		if _, err := c.List("", "*", nil).Collect(); err != nil {
			t.Fatalf("a failed re-check ended the session: %v", err)
		}
	})

	// …and a session whose credential is fine is left alone, however often
	// it is asked.
	t.Run("a live credential is never disturbed", func(t *testing.T) {
		h := newHarness(t, config.Config{})
		h.account(t, "acme", user, "bcdf-pw", "")
		h.ctrl.liveInterval.Store(int64(time.Millisecond))
		c := dial(t, h.addr)
		if err := c.Login(user, "bcdf-pw").Wait(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			time.Sleep(3 * time.Millisecond)
			if err := c.Noop().Wait(); err != nil {
				t.Fatalf("NOOP %d: %v", i, err)
			}
		}
	})
}
