// Package sweep is the continuation-store janitor: a single periodic
// background pass that fails abandoned/expired runs, fails runs whose
// resumer crashed mid-resume, and purges long-dead runs. It only ever
// reads the store and writes create-if-absent terminal docs (FailRun) or
// deletes whole finished runs (PurgeRun) — it never re-enters the
// processor and never mutates a live run.
package sweep

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

type SweeperController struct {
	ctx      context.Context
	pu       *processor.Unit
	shutdown chan bool
	wg       sync.WaitGroup
}

func NewController(ctx context.Context, pu *processor.Unit) *SweeperController {
	return &SweeperController{
		ctx:      ctx,
		pu:       pu,
		shutdown: make(chan bool),
	}
}

func (sc *SweeperController) enabled() bool {
	return sc.pu.Conf.ContinuationSweepPeriod > 0 && sc.pu.Runs != nil
}

func (sc *SweeperController) Start() {
	if !sc.enabled() {
		return
	}

	ctx, cancel := context.WithCancel(sc.ctx)
	sc.ctx = ctx

	go func() {
		sc.pu.Logger.Info("continuation sweeper started",
			zap.Int("period_s", sc.pu.Conf.ContinuationSweepPeriod),
			zap.Int("retention_s", sc.pu.Conf.ContinuationRetention),
			zap.Int("stale_resume_after_s", sc.pu.Conf.ContinuationStaleResumeAfter))
		sc.wg.Add(1)

		period := time.Duration(sc.pu.Conf.ContinuationSweepPeriod) * time.Second
		for {
			select {
			case <-time.After(period):
				sc.sweep(sc.ctx)
			case doshutdown := <-sc.shutdown:
				if doshutdown {
					cancel()
					sc.wg.Done()
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
	sc.pu.Logger.Info("calling continuation sweeper stop")
	sc.shutdown <- true
	sc.wg.Wait()
	sc.pu.Logger.Info("continuation sweeper stopped")
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
