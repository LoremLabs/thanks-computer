package dev

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/radovskyb/watcher"
)

// WatchOps watches dir recursively for changes to *.txcl and *.json
// files. Calls onChange with a 500ms-debounced cadence — bursty editor
// saves coalesce into a single re-apply rather than firing N times.
//
// Blocks until ctx is canceled. Returns the watcher's exit error (or
// nil on clean shutdown).
func WatchOps(ctx context.Context, dir string, debounce time.Duration, onChange func()) error {
	return watchDir(ctx, dir, debounce, relevantExt, onChange)
}

// WatchColocatedComputes watches the OPS/ tree for colocated compute source
// changes (.js/.ts) and fires onChange (debounced). Only compute source — not
// .txcl/.json, which the OPS draft watcher handles — so a compute edit
// triggers a rebuild+activate without double-firing on resonator/mock edits.
func WatchColocatedComputes(ctx context.Context, dir string, debounce time.Duration, onChange func()) error {
	return watchDir(ctx, dir, debounce, isComputeSource, onChange)
}

// watchDir is the shared recursive-watch loop. match decides which changed
// paths fire onChange.
func watchDir(ctx context.Context, dir string, debounce time.Duration, match func(string) bool, onChange func()) error {
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	w := watcher.New()
	w.SetMaxEvents(0)
	defer w.Close()

	if err := w.AddRecursive(dir); err != nil {
		return err
	}

	// Debounce: a flurry of saves within the window collapses into a
	// single onChange after the window quiets.
	var mu sync.Mutex
	var pending bool
	fire := func() {
		mu.Lock()
		if !pending {
			mu.Unlock()
			return
		}
		pending = false
		mu.Unlock()
		onChange()
	}
	schedule := func() {
		mu.Lock()
		alreadyPending := pending
		pending = true
		mu.Unlock()
		if alreadyPending {
			return
		}
		go func() {
			time.Sleep(debounce)
			fire()
		}()
	}

	go func() {
		for {
			select {
			case ev := <-w.Event:
				if ev.IsDir() {
					continue
				}
				name := ev.Path
				if !match(name) {
					continue
				}
				schedule()
			case <-w.Error:
				// transient errors from the watcher are non-fatal; ignore
			case <-ctx.Done():
				w.Close()
				return
			case <-w.Closed:
				return
			}
		}
	}()

	if err := w.Start(100 * time.Millisecond); err != nil {
		return err
	}
	return nil
}

func relevantExt(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".txcl" || ext == ".json"
}

// isComputeSource matches a compute project's editable source files but
// excludes the build/ output dir — otherwise writing the compiled wasm (and
// the wrapped entry) would re-trigger the very rebuild that produced them.
func isComputeSource(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".js", ".ts", ".mjs":
		return true
	}
	return false
}
