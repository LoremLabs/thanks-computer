package controlapply

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

// ackableSource wraps the file source and additionally records every
// Ack call so the test can assert ack-after-apply behavior.
type ackableSource struct {
	inner   feed.Source
	mu      sync.Mutex
	acked   []string
	failFor map[string]error
}

func (a *ackableSource) Name() string { return "ackable-test" }

func (a *ackableSource) Poll(ctx context.Context, since uint64) ([]controlevent.Event, error) {
	return a.inner.Poll(ctx, since)
}

func (a *ackableSource) Ack(_ context.Context, eventID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.failFor[eventID]; ok {
		return e
	}
	a.acked = append(a.acked, eventID)
	return nil
}

// TestApplierAcksAfterSuccessfulApply verifies the new feed.Acker
// path: a Source that implements Ack is called per-event AFTER the
// applier's local commit, and not before.
func TestApplierAcksAfterSuccessfulApply(t *testing.T) {
	h := newHarness(t)

	// Wrap the harness's file source in our ackable shim.
	ack := &ackableSource{inner: h.c.src}
	h.c.src = ack

	rows := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_ack", "slug": "ackme"}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/ack", data, []byte(`{}`))
	h.putEvent(t, "ack.json", controlevent.Event{
		EventID: "evt-ack-1",
		Type:    controlevent.TypeTenantCreated, ArtifactRef: "rows/ack",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 30,
	})

	h.c.pollOnce(context.Background())

	if h.cursor(t) != 30 {
		t.Fatalf("cursor not advanced: %d", h.cursor(t))
	}
	ack.mu.Lock()
	defer ack.mu.Unlock()
	if len(ack.acked) != 1 || ack.acked[0] != "evt-ack-1" {
		t.Errorf("Ack not called or wrong event_id: %v", ack.acked)
	}
}

// TestApplierAckFailureDoesNotHalt confirms that a failed Ack is
// loud-logged but doesn't roll back the apply or stop processing the
// next event in the batch. Subsequent fetches will see the un-acked
// event redelivered; applied_events catches the replay.
func TestApplierAckFailureDoesNotHalt(t *testing.T) {
	h := newHarness(t)
	ack := &ackableSource{
		inner:   h.c.src,
		failFor: map[string]error{"evt-ack-fail-1": errors.New("simulated broker ack outage")},
	}
	h.c.src = ack

	rows1 := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_ack_a", "slug": "a"}}}
	data1, _ := json.Marshal(rows1)
	_ = h.astore.Put(context.Background(), "rows/ack-a", data1, []byte(`{}`))

	rows2 := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_ack_b", "slug": "b"}}}
	data2, _ := json.Marshal(rows2)
	_ = h.astore.Put(context.Background(), "rows/ack-b", data2, []byte(`{}`))

	h.putEvent(t, "a.json", controlevent.Event{
		EventID: "evt-ack-fail-1", Type: controlevent.TypeTenantCreated,
		ArtifactRef: "rows/ack-a", Checksum: "sha256:" + sha256Hex(data1),
		ControlVersion: 40,
	})
	h.putEvent(t, "b.json", controlevent.Event{
		EventID: "evt-ack-ok-2", Type: controlevent.TypeTenantCreated,
		ArtifactRef: "rows/ack-b", Checksum: "sha256:" + sha256Hex(data2),
		ControlVersion: 41,
	})

	h.c.pollOnce(context.Background())

	// Both rows applied — ack failure doesn't roll back the local commit.
	for _, id := range []string{"tnt_ack_a", "tnt_ack_b"} {
		var slug string
		if err := h.db.QueryRow(`SELECT slug FROM tenants WHERE tenant_id = ?`, id).Scan(&slug); err != nil {
			t.Errorf("expected tenant %s to be applied despite ack failure: %v", id, err)
		}
	}
	if h.cursor(t) != 41 {
		t.Errorf("cursor=%d; both events should have committed (Ack failure ≠ apply failure)", h.cursor(t))
	}
	ack.mu.Lock()
	defer ack.mu.Unlock()
	// Second event was acked (Ack failure on the first didn't poison the loop).
	if len(ack.acked) != 1 || ack.acked[0] != "evt-ack-ok-2" {
		t.Errorf("expected only evt-ack-ok-2 in acked list, got %v", ack.acked)
	}
}
