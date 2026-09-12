package workspace

import (
	"context"
	"testing"
	"time"
)

func TestLeaseRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	wsID := ID("acme", "app", "tools")
	l := Lease{
		ID: "att_1", Tenant: "acme", Stack: "app", Name: "tools", Kind: "pty",
		SessionID: "ws_1", NodeID: "node-a", RunID: "run-1",
		StartedAt: base, ExpiresAt: base.Add(8 * time.Hour),
	}
	if err := s.InsertLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLease(ctx, "att_1")
	if err != nil || got == nil {
		t.Fatalf("GetLease = %+v, %v", got, err)
	}
	if got.WorkspaceID != wsID || got.RunID != "run-1" || !got.LastHeartbeat.Equal(base) || got.ReleasedAt != nil {
		t.Errorf("lease = %+v", got)
	}
	if !got.Live(base.Add(30 * time.Second)) {
		t.Error("fresh lease should be live")
	}

	// Live at +1m59s after a heartbeat at +1m; stale two intervals later.
	if err := s.Heartbeat(ctx, "att_1", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	live, err := s.ListLive(ctx, wsID, base.Add(time.Minute+59*time.Second))
	if err != nil || len(live) != 1 {
		t.Fatalf("ListLive after heartbeat = %v, %v", live, err)
	}
	live, _ = s.ListLive(ctx, wsID, base.Add(3*time.Minute+time.Second))
	if len(live) != 0 {
		t.Errorf("a lease two heartbeats silent must not be live: %v", live)
	}

	// Expiry ends it even with a fresh heartbeat.
	if err := s.Heartbeat(ctx, "att_1", base.Add(8*time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, _ = s.ListLive(ctx, wsID, base.Add(8*time.Hour+time.Second))
	if len(live) != 0 {
		t.Errorf("an expired lease must not be live: %v", live)
	}

	// Release is recorded once and a released lease neither heartbeats nor lists as live.
	rel := base.Add(2 * time.Hour)
	if err := s.ReleaseLease(ctx, "att_1", rel); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseLease(ctx, "att_1", rel.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, "att_1", rel.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetLease(ctx, "att_1")
	if got.ReleasedAt == nil || !got.ReleasedAt.Equal(rel) {
		t.Errorf("released_at = %v, want %v", got.ReleasedAt, rel)
	}
	if !got.LastHeartbeat.Equal(base.Add(8 * time.Hour)) {
		t.Errorf("heartbeat after release must be ignored; got %v", got.LastHeartbeat)
	}
	all, err := s.ListLeases(ctx, "acme", false, 0)
	if err != nil || len(all) != 0 {
		t.Errorf("ListLeases live-only = %v, %v", all, err)
	}
	all, _ = s.ListLeases(ctx, "", true, 0)
	if len(all) != 1 {
		t.Errorf("ListLeases including released = %v", all)
	}
}

func TestInsertLeaseValidates(t *testing.T) {
	s := newTestStore(t)
	if err := s.InsertLease(context.Background(), Lease{ID: "x", Tenant: "acme"}); err == nil {
		t.Fatal("insert without stack/name/kind/session/node should fail")
	}
}
