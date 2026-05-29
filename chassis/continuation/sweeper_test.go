package continuation_test

import (
	"context"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
)

func TestListRunIDs(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))

	id1, _, err := r.CreateRun(ctx, "acme", "boot", "h1", "boot/0", "rid-1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun 1: %v", err)
	}
	id2, _, err := r.CreateRun(ctx, "acme", "boot", "h2", "boot/0", "rid-2", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	// Add nested docs so List returns many keys for one run; ListRunIDs
	// must still dedupe to the distinct run id.
	if err := r.SuspendStage(ctx, id1, "boot/300", "{}", "h1", nil); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}

	ids, err := r.ListRunIDs(ctx)
	if err != nil {
		t.Fatalf("ListRunIDs: %v", err)
	}
	got := map[string]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[id1] || !got[id2] {
		t.Fatalf("ListRunIDs = %v, want both %s and %s", ids, id1, id2)
	}
	if len(ids) != 2 {
		t.Fatalf("ListRunIDs len = %d, want 2 (deduped)", len(ids))
	}
}

func TestReadResumeClaim(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))
	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "", time.Now().Add(time.Hour))

	if _, exists, err := r.ReadResumeClaim(ctx, id, "boot/300"); err != nil || exists {
		t.Fatalf("unclaimed: exists=%v err=%v, want false/nil", exists, err)
	}
	won, err := r.ClaimResume(ctx, id, "boot/300")
	if err != nil || !won {
		t.Fatalf("ClaimResume: won=%v err=%v", won, err)
	}
	at, exists, err := r.ReadResumeClaim(ctx, id, "boot/300")
	if err != nil || !exists {
		t.Fatalf("claimed: exists=%v err=%v, want true/nil", exists, err)
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("claimed_at = %v, want ~now", at)
	}
}

func TestPurgeRun(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))

	id, rcid, err := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "origin-rid", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := r.SuspendStage(ctx, id, "boot/300", "{}", "", []continuation.OpManifestEntry{{Ordinal: 0, Op: "x", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	if err := r.CreateOpRecords(ctx, id, "boot/300", []continuation.OpRecordSpec{{
		Ordinal: 0, Op: "x", Async: true, OpContinuationID: "opc_test", TokenHash: "h", Input: []byte("{}"),
	}}); err != nil {
		t.Fatalf("CreateOpRecords: %v", err)
	}

	if err := r.PurgeRun(ctx, id); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}

	if _, err := r.ReadRunCreated(ctx, id); err != continuation.ErrNotFound {
		t.Fatalf("ReadRunCreated after purge = %v, want ErrNotFound", err)
	}
	if _, err := r.ResolveRunContinuation(ctx, rcid); err != continuation.ErrNotFound {
		t.Fatalf("ResolveRunContinuation after purge = %v, want ErrNotFound", err)
	}
	if _, err := r.ResolveRequestContinuation(ctx, "origin-rid"); err != continuation.ErrNotFound {
		t.Fatalf("ResolveRequestContinuation after purge = %v, want ErrNotFound", err)
	}
	if _, err := r.ResolveOpContinuation(ctx, "opc_test"); err != continuation.ErrNotFound {
		t.Fatalf("ResolveOpContinuation after purge = %v, want ErrNotFound", err)
	}
	ids, _ := r.ListRunIDs(ctx)
	for _, gid := range ids {
		if gid == id {
			t.Fatalf("ListRunIDs still contains purged run %s", id)
		}
	}
	// Idempotent: a second purge of an already-gone run is a no-op.
	if err := r.PurgeRun(ctx, id); err != nil {
		t.Fatalf("PurgeRun (repeat) = %v, want nil", err)
	}
}

func TestOpstackSnapshotImmutable(t *testing.T) {
	ctx := context.Background()
	r := continuation.NewRuns(newStore(t))
	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "", time.Now().Add(time.Hour))

	if _, err := r.ReadOpstackSnapshot(ctx, id); err != continuation.ErrNotFound {
		t.Fatalf("ReadOpstackSnapshot (absent) = %v, want ErrNotFound", err)
	}
	if err := r.WriteOpstackSnapshot(ctx, id, []byte(`{"ops":[{"stack":"boot","scope":300,"name":"x","txcl":"v1"}]}`)); err != nil {
		t.Fatalf("WriteOpstackSnapshot: %v", err)
	}
	// Create-if-absent: a second write must NOT overwrite (a re-suspend
	// of a multi-stage run resumes against the original frozen opstack).
	if err := r.WriteOpstackSnapshot(ctx, id, []byte(`{"ops":[{"txcl":"v2"}]}`)); err != nil {
		t.Fatalf("WriteOpstackSnapshot (repeat): %v", err)
	}
	b, err := r.ReadOpstackSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("ReadOpstackSnapshot: %v", err)
	}
	if want := `{"ops":[{"stack":"boot","scope":300,"name":"x","txcl":"v1"}]}`; string(b) != want {
		t.Fatalf("snapshot = %s, want original %s", b, want)
	}
}
