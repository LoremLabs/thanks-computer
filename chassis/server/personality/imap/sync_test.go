package imap

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// These tests simulate a shared index: a second Store over the same file,
// with no change listener, stands in for an op on another node. The head
// must deliver what it wrote on the client's next command and, in IDLE,
// within --imap-sync-interval.

func seqs(u []uint32) string {
	parts := make([]string, 0, len(u))
	for _, n := range u {
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, ",")
}

func selectINBOX(t *testing.T, h *harness) (*imapclient.Client, *unilateral) {
	t.Helper()
	c, u := dialWith(t, h.addr)
	if err := c.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	return c, u
}

func TestRemoteAppendSeenOnNextCommand(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	c, u := selectINBOX(t, h)
	remote := h.remote(t)
	h.appendHelloVia(t, remote, "acme", "paris@example.com", "r1", "remote one", "one")
	h.appendHelloVia(t, remote, "acme", "paris@example.com", "r2", "remote two", "two")
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := c.Mailbox().NumMessages; n != 2 {
		t.Fatalf("EXISTS after remote appends = %d", n)
	}
	exists, _, _ := u.snapshot()
	if seqs(exists) != "1,2" {
		t.Errorf("EXISTS sequence = %s, want 1,2 (one per row)", seqs(exists))
	}
	msgs, err := c.Fetch(imap.SeqSet{{Start: 1, Stop: 2}}, &imap.FetchOptions{UID: true, Envelope: true}).Collect()
	if err != nil || len(msgs) != 2 || msgs[0].UID != 1 || msgs[1].Envelope.Subject != "remote two" {
		t.Errorf("fetch = %+v err=%v", msgs, err)
	}
}

func TestRemoteFlagsDelivered(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	h.appendHello(t, "acme", "paris@example.com", "k", "s", "t")
	c, u := selectINBOX(t, h)
	remote := h.remote(t)
	mb, _, _ := remote.GetMailbox(context.Background(), "acme", "paris@example.com", "INBOX")
	if _, err := remote.SetFlags(context.Background(), mb.ID, 1, []string{`\Flagged`, "$Hello"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	got := u.waitFetched(t, 1)
	if got[0].SeqNum != 1 || !hasFlag(got[0].Flags, imap.FlagFlagged) {
		t.Errorf("unilateral FETCH = %+v", got[0])
	}
}

func TestRemoteExpungeSequence(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	for _, k := range []string{"a", "b", "c"} {
		h.appendHello(t, "acme", "paris@example.com", k, k, k)
	}
	c, u := selectINBOX(t, h)
	remote := h.remote(t)
	ctx := context.Background()
	mb, _, _ := remote.GetMailbox(ctx, "acme", "paris@example.com", "INBOX")
	if _, err := remote.RemoveMessage(ctx, mb.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	_, exp, _ := u.snapshot()
	if seqs(exp) != "2" || c.Mailbox().NumMessages != 2 {
		t.Fatalf("expunged = %s exists=%d", seqs(exp), c.Mailbox().NumMessages)
	}
	msgs, _ := c.Fetch(imap.SeqSetNum(2), &imap.FetchOptions{UID: true}).Collect()
	if len(msgs) != 1 || msgs[0].UID != 3 {
		t.Errorf("seq 2 = %+v, want uid 3", msgs)
	}
	if _, err := remote.RemoveMessage(ctx, mb.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.RemoveMessage(ctx, mb.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	exists, exp, _ := u.snapshot()
	if seqs(exp) != "2,2,1" || c.Mailbox().NumMessages != 0 {
		t.Errorf("expunged = %s (want 2 then 2,1) exists=%d", seqs(exp), c.Mailbox().NumMessages)
	}
	for _, n := range exists {
		if n == 0 {
			t.Error("EXISTS 0 was sent")
		}
	}
}

func TestOwnStoreNotEchoed(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	h.appendHello(t, "acme", "paris@example.com", "k", "s", "t")
	c1, u1 := selectINBOX(t, h)
	c2, u2 := selectINBOX(t, h)
	if _, err := c1.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	if err := c1.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c2.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	got := u2.waitFetched(t, 1)
	if !hasFlag(got[0].Flags, imap.FlagSeen) {
		t.Errorf("other session's FETCH = %+v", got[0])
	}
	time.Sleep(100 * time.Millisecond) // give a wrongly-echoed FETCH time to land
	if _, _, f := u1.snapshot(); f != 0 {
		t.Errorf("writer received %d unilateral FETCH(es) for its own STORE", f)
	}
}

func TestIdleTickDeliversRemoteAppend(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "1s"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	c, u := selectINBOX(t, h)
	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}
	remote := h.remote(t)
	h.appendHelloVia(t, remote, "acme", "paris@example.com", "r", "idle", "x")
	deadline := time.Now().Add(4 * time.Second)
	for {
		exists, _, _ := u.snapshot()
		if len(exists) == 1 && exists[0] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no EXISTS during IDLE within the tick; got %v", exists)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteResetSendsBye(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	h.appendHello(t, "acme", "paris@example.com", "k", "s", "t")
	c, _ := dialWith(t, h.addr)
	if err := c.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	first, err := c.Select("INBOX", nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	oldValidity := first.UIDValidity
	remote := h.remote(t)
	ctx := context.Background()
	mb, _, _ := remote.GetMailbox(ctx, "acme", "paris@example.com", "INBOX")
	if _, err := remote.ResetMailbox(ctx, mb.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err == nil {
		t.Error("NOOP after a reset should fail (BYE)")
	}
	c2 := dial(t, h.addr)
	if err := c2.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	sel, err := c2.Select("INBOX", nil).Wait()
	if err != nil || sel.UIDValidity == oldValidity || sel.NumMessages != 0 {
		t.Errorf("reselect = %+v err=%v (old validity %d)", sel, err, oldValidity)
	}
}

func TestMoveExpungeStillWriteExpunges(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSyncInterval: "0"})
	h.account(t, "acme", "paris@example.com", "pw", "")
	for _, k := range []string{"a", "b", "c", "d"} {
		h.appendHello(t, "acme", "paris@example.com", k, k, k)
	}
	c, u := selectINBOX(t, h)
	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Store(imap.SeqSet{{Start: 1, Stop: 1}, {Start: 3, Stop: 3}}, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}, Silent: true}, nil).Collect(); err != nil {
		t.Fatal(err)
	}
	// EXPUNGE responses during the EXPUNGE command go to its collector.
	exp, err := c.Expunge().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if seqs(exp) != "3,1" || c.Mailbox().NumMessages != 2 {
		t.Fatalf("EXPUNGE responses = %s exists=%d", seqs(exp), c.Mailbox().NumMessages)
	}
	// During a native MOVE they are unilateral.
	md, err := c.Move(imap.SeqSetNum(1), "Archive").Wait()
	if err != nil || md.UIDValidity == 0 {
		t.Fatalf("move = %+v err=%v", md, err)
	}
	_, uexp, _ := u.snapshot()
	if seqs(uexp) != "1" || c.Mailbox().NumMessages != 1 {
		t.Errorf("after MOVE: unilateral expunged = %s exists=%d", seqs(uexp), c.Mailbox().NumMessages)
	}
}
