package sweep

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

func harness(t *testing.T, cfg config.Config) (*SweeperController, *continuation.Runs) {
	t.Helper()
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore.New: %v", err)
	}
	runs := continuation.NewRuns(fs)
	pu := &processor.Unit{Conf: cfg, Logger: zap.NewNop(), Runs: runs}
	return NewController(context.Background(), pu), runs
}

func mustState(t *testing.T, r *continuation.Runs, id, want string) {
	t.Helper()
	got, err := r.RunState(context.Background(), id)
	if err != nil {
		t.Fatalf("RunState(%s): %v", id, err)
	}
	if got != want {
		t.Fatalf("RunState(%s) = %q, want %q", id, got, want)
	}
}

func TestSweepExpiresAbandonedRun(t *testing.T) {
	ctx := context.Background()
	sc, r := harness(t, config.Config{ContinuationSweepPeriod: 1, ContinuationRetention: 604800, ContinuationStaleResumeAfter: 600})

	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "", time.Now().Add(-time.Minute))
	if err := r.SuspendStage(ctx, id, "boot/300", "{}", "", []continuation.OpManifestEntry{{Ordinal: 0, Op: "x", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	mustState(t, r, id, continuation.StateWaiting)

	sc.sweep(ctx)
	mustState(t, r, id, continuation.StateFailed)
	// Idempotent: a second pass changes nothing.
	sc.sweep(ctx)
	mustState(t, r, id, continuation.StateFailed)
}

func TestSweepFailsStaleResumer(t *testing.T) {
	ctx := context.Background()
	// staleResumeAfter=0 ⇒ any existing claim is treated as crashed.
	sc, r := harness(t, config.Config{ContinuationSweepPeriod: 1, ContinuationRetention: 604800, ContinuationStaleResumeAfter: 0})

	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "", time.Now().Add(time.Hour))
	stage := "boot/300"
	if err := r.SuspendStage(ctx, id, stage, "{}", "", []continuation.OpManifestEntry{{Ordinal: 0, Op: "x", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}
	if _, err := r.RecordTerminal(ctx, id, stage, 0, "x", "completed", []byte("{}")); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	mustState(t, r, id, continuation.StateResumable)
	won, err := r.ClaimResume(ctx, id, stage)
	if err != nil || !won {
		t.Fatalf("ClaimResume: won=%v err=%v", won, err)
	}
	time.Sleep(2 * time.Millisecond) // ensure now-claimedAt > 0

	sc.sweep(ctx)
	mustState(t, r, id, continuation.StateFailed)
}

func TestSweepPurgesDeadRun(t *testing.T) {
	ctx := context.Background()
	sc, r := harness(t, config.Config{ContinuationSweepPeriod: 1, ContinuationRetention: 0, ContinuationStaleResumeAfter: 600})

	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "rid", time.Now().Add(-time.Minute))
	if err := r.WriteResult(ctx, id, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	mustState(t, r, id, continuation.StateCompleted)

	sc.sweep(ctx)
	if _, err := r.ReadRunCreated(ctx, id); err != continuation.ErrNotFound {
		t.Fatalf("ReadRunCreated after purge = %v, want ErrNotFound", err)
	}
}

func TestSweepLeavesLiveRun(t *testing.T) {
	ctx := context.Background()
	sc, r := harness(t, config.Config{ContinuationSweepPeriod: 1, ContinuationRetention: 604800, ContinuationStaleResumeAfter: 600})

	id, _, _ := r.CreateRun(ctx, "acme", "boot", "", "boot/0", "", time.Now().Add(time.Hour))
	if err := r.SuspendStage(ctx, id, "boot/300", "{}", "", []continuation.OpManifestEntry{{Ordinal: 0, Op: "x", Async: true}}); err != nil {
		t.Fatalf("SuspendStage: %v", err)
	}

	sc.sweep(ctx)
	mustState(t, r, id, continuation.StateWaiting)
}

func TestDisabledSweeperStartStopNoop(t *testing.T) {
	sc, _ := harness(t, config.Config{ContinuationSweepPeriod: 0})
	if sc.enabled() {
		t.Fatal("enabled() = true with period 0, want false")
	}
	// Must not block or panic when disabled.
	sc.Start()
	sc.Stop()
}
