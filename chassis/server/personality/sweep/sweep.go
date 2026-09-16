// Package sweep is the store janitor: a single periodic background pass
// per store. For the continuation store it fails abandoned/expired runs,
// fails runs whose resumer crashed mid-resume, and purges long-dead runs;
// it only ever reads the store and writes create-if-absent terminal docs
// (FailRun) or deletes whole finished runs (PurgeRun) — it never re-enters
// the processor and never mutates a live run. For the drive store (where
// the `webdav` head runs) it reclaims unreferenced objects and hard-deletes
// old tombstones (drive.Store.Sweep).
package sweep

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

type SweeperController struct {
	ctx      context.Context
	pu       *processor.Unit
	drive    *drive.Store // nil ⇒ no drive sweep
	shutdown chan bool
	wg       sync.WaitGroup
}

// NewController constructs the sweeper. driveStore may be nil; the drive
// arm runs only where the `webdav` head runs (the node that serves a
// collection is the one that pays for its housekeeping).
func NewController(ctx context.Context, pu *processor.Unit, driveStore *drive.Store) *SweeperController {
	return &SweeperController{
		ctx:      ctx,
		pu:       pu,
		drive:    driveStore,
		shutdown: make(chan bool),
	}
}

func (sc *SweeperController) continuationEnabled() bool {
	return sc.pu.Conf.ContinuationSweepPeriod > 0 && sc.pu.Runs != nil
}

func (sc *SweeperController) driveEnabled() bool {
	return sc.drive != nil && sc.pu.Conf.DriveSweepPeriod > 0 && sc.pu.Conf.HasPersonality("webdav")
}

func (sc *SweeperController) enabled() bool {
	return sc.continuationEnabled() || sc.driveEnabled()
}

// never is a channel that never fires, for a disabled arm.
var never = make(chan time.Time)

func (sc *SweeperController) Start() {
	if !sc.enabled() {
		return
	}

	ctx, cancel := context.WithCancel(sc.ctx)
	sc.ctx = ctx
	sc.wg.Add(1)

	go func() {
		defer sc.wg.Done()
		var contPeriod, drivePeriod time.Duration
		if sc.continuationEnabled() {
			contPeriod = time.Duration(sc.pu.Conf.ContinuationSweepPeriod) * time.Second
			sc.pu.Logger.Info("continuation sweeper started",
				zap.Int("period_s", sc.pu.Conf.ContinuationSweepPeriod),
				zap.Int("retention_s", sc.pu.Conf.ContinuationRetention),
				zap.Int("stale_resume_after_s", sc.pu.Conf.ContinuationStaleResumeAfter))
		}
		if sc.driveEnabled() {
			drivePeriod = time.Duration(sc.pu.Conf.DriveSweepPeriod) * time.Second
			sc.pu.Logger.Info("drive sweeper started",
				zap.Int("period_s", sc.pu.Conf.DriveSweepPeriod),
				zap.Int("grace_s", sc.pu.Conf.DriveSweepGrace),
				zap.Int("tombstone_retention_s", sc.pu.Conf.DriveTombstoneRetention))
		}
		after := func(d time.Duration) <-chan time.Time {
			if d <= 0 {
				return never
			}
			return time.After(d)
		}
		contTick := after(contPeriod)
		driveTick := after(drivePeriod)
		for {
			select {
			case <-contTick:
				sc.sweep(sc.ctx)
				contTick = after(contPeriod)
			case <-driveTick:
				sc.sweepDrive(sc.ctx)
				driveTick = after(drivePeriod)
			case doshutdown := <-sc.shutdown:
				if doshutdown {
					cancel()
					return
				}
			}
		}
	}()
}

func (sc *SweeperController) Stop() {
	if !sc.enabled() {
		return
	}
	sc.pu.Logger.Info("calling sweeper stop")
	sc.shutdown <- true
	sc.wg.Wait()
	sc.pu.Logger.Info("sweeper stopped")
}

// sweepDrive is one drive pass: unreferenced objects older than the grace
// window and tombstones past retention.
func (sc *SweeperController) sweepDrive(ctx context.Context) {
	rep, err := sc.drive.Sweep(ctx,
		time.Duration(sc.pu.Conf.DriveSweepGrace)*time.Second,
		time.Duration(sc.pu.Conf.DriveTombstoneRetention)*time.Second)
	fields := []zap.Field{
		zap.Int("scanned", rep.Scanned),
		zap.Int("orphans", rep.Orphans),
		zap.Int64("orphan_bytes", rep.OrphanBytes),
		zap.Int64("tombstones", rep.Tombstones),
		zap.Int64("collections", rep.Collections),
		zap.Int("errors", rep.Errors),
	}
	switch {
	case err != nil:
		sc.pu.Logger.Warn("drive sweep error", append(fields, zap.Error(err))...)
	case rep.Orphans+int(rep.Tombstones)+int(rep.Collections)+rep.Errors > 0:
		sc.pu.Logger.Info("drive sweep", fields...)
	default:
		sc.pu.Logger.Debug("drive sweep", fields...)
	}
}

// sweep is one full pass. Every transition is create-if-absent or a
// whole-run delete, so the pass is idempotent and safe to repeat.
func (sc *SweeperController) sweep(ctx context.Context) {
	runs := sc.pu.Runs
	ids, err := runs.ListRunIDs(ctx)
	if err != nil {
		sc.pu.Logger.Warn("continuation sweep list error", zap.Error(err))
		return
	}

	now := time.Now().UTC()
	retention := time.Duration(sc.pu.Conf.ContinuationRetention) * time.Second
	staleAfter := time.Duration(sc.pu.Conf.ContinuationStaleResumeAfter) * time.Second

	var scanned, expired, staled, purged int
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return
		default:
		}
		scanned++

		rc, rcErr := runs.ReadRunCreated(ctx, id)
		if rcErr != nil {
			// run-created absent (e.g. mid-purge from a prior pass) — finish
			// the purge so no orphan dir lingers, then move on.
			_ = runs.PurgeRun(ctx, id)
			continue
		}
		state, sErr := runs.RunState(ctx, id)
		if sErr != nil {
			continue
		}
		terminal := state == continuation.StateCompleted || state == continuation.StateFailed

		// 3. Purge: terminal and well past expiry.
		if terminal {
			if !rc.ExpiresAt.IsZero() && now.After(rc.ExpiresAt.Add(retention)) {
				if err := runs.PurgeRun(ctx, id); err != nil {
					sc.pu.Logger.Warn("continuation purge error",
						zap.String("run", id), zap.Error(err))
				} else {
					purged++
				}
			}
			continue
		}

		// 1. Expire: non-terminal and past its TTL — nobody is coming back.
		if !rc.ExpiresAt.IsZero() && now.After(rc.ExpiresAt) {
			_ = runs.FailRun(ctx, id, "expired")
			_ = runs.AppendEvent(ctx, id, "run.expired",
				map[string]any{"expires_at": rc.ExpiresAt})
			expired++
			continue
		}

		// 2. Crashed resumer: the current stage is ready to advance and a
		// resume-claim has sat far longer than any legitimate resume — the
		// resumer won the claim then died, and no callback will re-fire.
		// Fail it cleanly so the polling client stops waiting.
		cur, ok, cErr := runs.CurrentStage(ctx, id)
		if cErr != nil || !ok {
			continue
		}
		stageState, stErr := runs.StageState(ctx, id, cur.Stage, cur.Manifest)
		if stErr != nil || stageState != continuation.StateResumable {
			continue
		}
		claimedAt, claimed, clErr := runs.ReadResumeClaim(ctx, id, cur.Stage)
		if clErr != nil || !claimed {
			continue
		}
		if now.Sub(claimedAt) > staleAfter {
			_ = runs.FailRun(ctx, id, "resumer-stale")
			_ = runs.AppendEvent(ctx, id, "run.resumer_stale",
				map[string]any{"stage": cur.Stage})
			staled++
		}
	}

	fields := []zap.Field{
		zap.Int("scanned", scanned),
		zap.Int("expired", expired),
		zap.Int("resumer_stale", staled),
		zap.Int("purged", purged),
	}
	if expired+staled+purged > 0 {
		sc.pu.Logger.Info("continuation sweep", fields...)
	} else {
		sc.pu.Logger.Debug("continuation sweep", fields...)
	}
}
