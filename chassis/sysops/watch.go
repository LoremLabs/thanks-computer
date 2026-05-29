package sysops

import (
	"context"
	"path/filepath"
	"time"

	"github.com/bep/debounce"
	"github.com/radovskyb/watcher"
	"go.uber.org/zap"
)

// Watch hot-recompiles the system bundle when files under cfg.Dir
// change. It is wired ONLY on the `txco dev` path (gated by
// --system-opstacks-watch); `txco serve` compiles once at startup and
// stays static. On each debounced change it re-Loads (re-validating
// txcl) and, on success, hands the fresh *Loader to reapply — which
// the caller overlays onto the live snapshot. A failed Load (bad edit)
// is logged and the previous bundle stays live, mirroring the
// non-fatal dbcache OnReload contract.
//
// Same watcher/debounce stack the dbcache uses, for consistency.
func Watch(ctx context.Context, cfg Config, logger *zap.Logger, reapply func(*Loader) error) {
	if cfg.Dir == "" {
		return
	}
	w := watcher.New()
	w.SetMaxEvents(1)

	go func() {
		debounced := debounce.New(500 * time.Millisecond)
		for {
			select {
			case <-w.Event:
				debounced(func() {
					l, err := Load(cfg)
					if err != nil {
						logger.Error("sysops watch: reload failed; keeping previous bundle",
							zap.String("dir", cfg.Dir), zap.String("err", err.Error()))
						return
					}
					if err := reapply(l); err != nil {
						logger.Error("sysops watch: reapply failed",
							zap.String("err", err.Error()))
						return
					}
					logger.Info("sysops watch: system opstacks reloaded",
						zap.String("dir", cfg.Dir))
				})
			case err := <-w.Error:
				logger.Warn("sysops watch error", zap.String("err", err.Error()))
			case <-w.Closed:
				return
			case <-ctx.Done():
				w.Close()
				return
			}
		}
	}()

	// Watch only the OPS/ subtree (cfg.Dir is the workspace root;
	// recursing the whole thing would sweep node_modules/.git/APPS).
	opsDir := filepath.Join(cfg.Dir, "OPS")
	if err := w.AddRecursive(opsDir); err != nil {
		logger.Warn("sysops watch: cannot watch OPS dir; hot-reload disabled",
			zap.String("dir", opsDir), zap.String("err", err.Error()))
		return
	}
	go func() {
		if err := w.Start(time.Second); err != nil {
			logger.Warn("sysops watch: start failed", zap.String("err", err.Error()))
		}
	}()
}
