package room

import (
	"sync"
	"testing"
)

// fakeRelay records what the Hub relays out and lets a test inject inbound.
type fakeRelay struct {
	mu        sync.Mutex
	published []relayed
	closed    bool
}

type relayed struct {
	tenant, room string
	ev           Event
}

func (f *fakeRelay) Publish(tenant, room string, ev Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, relayed{tenant, room, ev})
}

func (f *fakeRelay) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

func TestHubPublishRelaysOut(t *testing.T) {
	h := NewHub(10)
	fr := &fakeRelay{}
	h.SetRelay(fr)
	ev := h.Publish("acme", "support", "matt", "hi", "msg_1")

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.published) != 1 {
		t.Fatalf("relayed %d events, want 1", len(fr.published))
	}
	got := fr.published[0]
	if got.tenant != "acme" || got.room != "support" || got.ev.Seq != ev.Seq || got.ev.Text != "hi" {
		t.Fatalf("relayed %+v, want acme/support seq=%d text=hi", got, ev.Seq)
	}
}

func TestHubDeliverInjectsToKnownRoomWithoutLooping(t *testing.T) {
	h := NewHub(10)
	fr := &fakeRelay{}
	h.SetRelay(fr)
	// A local subscriber makes the room "known" on this node.
	ch, _, unsub := h.Subscribe("acme", "support")
	defer unsub()

	// A remote event (from another node) arrives via Deliver, carrying its
	// origin seq — the Hub must re-sequence it locally.
	h.Deliver("acme", "support", Event{Actor: "_room", Text: "echo: hi", Seq: 99})
	select {
	case ev := <-ch:
		if ev.Text != "echo: hi" || ev.Actor != "_room" {
			t.Fatalf("got %+v", ev)
		}
		if ev.Seq == 99 {
			t.Fatal("Deliver must re-sequence locally, got the origin seq 99")
		}
	default:
		t.Fatal("expected the remote event delivered to the local subscriber")
	}

	// Deliver must NOT relay (no loop back to the fleet).
	if n := fr.count(); n != 0 {
		t.Fatalf("Deliver re-relayed %d events; it must not loop", n)
	}
}

func TestHubDeliverDropsUnwatchedRoom(t *testing.T) {
	h := NewHub(10)
	// No subscriber and no local publish → the room is unknown on this node.
	h.Deliver("acme", "nobody-here", Event{Actor: "x", Text: "drop me"})
	// Subscribing afterward must see no backfill — Deliver did not materialize
	// state for a room this node has no interest in.
	_, backfill, unsub := h.Subscribe("acme", "nobody-here")
	defer unsub()
	if len(backfill) != 0 {
		t.Fatalf("Deliver materialized state for an unwatched room; backfill=%d, want 0", len(backfill))
	}
}
