package room

import "testing"

func TestHubPublishSubscribe(t *testing.T) {
	h := NewHub(10)
	ch, backfill, unsub := h.Subscribe("acme", "support")
	defer unsub()
	if len(backfill) != 0 {
		t.Fatalf("backfill = %d, want 0 for a fresh room", len(backfill))
	}
	h.Publish("acme", "support", "matt", "hello", "msg_1")
	select {
	case ev := <-ch:
		if ev.Actor != "matt" || ev.Text != "hello" || ev.Seq != 1 || ev.MessageID != "msg_1" {
			t.Fatalf("got %+v", ev)
		}
	default:
		t.Fatal("expected a live event")
	}
}

func TestHubBackfillRespectsRingCap(t *testing.T) {
	h := NewHub(2) // keep only the last 2
	h.Publish("acme", "support", "a", "1", "")
	h.Publish("acme", "support", "b", "2", "")
	h.Publish("acme", "support", "c", "3", "")
	_, backfill, unsub := h.Subscribe("acme", "support")
	defer unsub()
	if len(backfill) != 2 {
		t.Fatalf("backfill = %d, want 2 (ring cap)", len(backfill))
	}
	if backfill[0].Text != "2" || backfill[1].Text != "3" {
		t.Fatalf("backfill texts = [%s %s], want [2 3]", backfill[0].Text, backfill[1].Text)
	}
}

func TestHubIsolatesTenantAndRoom(t *testing.T) {
	h := NewHub(10)
	ch, _, unsub := h.Subscribe("acme", "support")
	defer unsub()
	h.Publish("acme", "billing", "x", "other-room", "")  // same tenant, different room
	h.Publish("beta", "support", "x", "other-tenant", "") // same room, different tenant
	select {
	case ev := <-ch:
		t.Fatalf("subscriber leaked an event from another (tenant,room): %+v", ev)
	default:
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub(10)
	ch, _, unsub := h.Subscribe("acme", "support")
	unsub()
	// Publishing after unsubscribe must not panic, and the channel is closed.
	h.Publish("acme", "support", "x", "after-unsub", "")
	if _, ok := <-ch; ok {
		t.Fatal("expected a closed channel after unsubscribe")
	}
	unsub() // idempotent
}
