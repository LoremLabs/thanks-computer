package dev

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/radovskyb/watcher"
)

// Options configures the dev watchers. The zero value is valid (500ms
// debounce, FILES/ excluded, no extra ignores).
type Options struct {
	// Debounce coalesces a flurry of saves into a single onChange. <=0 → 500ms.
	Debounce time.Duration

	// Ignore is a list of glob patterns whose matching directories (and their
	// subtrees) are pruned from the watch. A pattern with a "/" is matched with
	// path.Match against the directory's path relative to the watched root
	// (e.g. "publications/*/FILES"); a pattern with no "/" matches a directory's
	// base name anywhere in the tree (e.g. "node_modules", "sources").
	Ignore []string

	// IncludeFiles, when false (the default), also prunes every per-stack
	// FILES/ asset tree. FILES/ holds built assets — not editable source — and
	// can be tens of thousands of files; a POLLING watcher lstat'ing them
	// 10×/sec is the main idle-CPU cost in a large workspace. Set true to watch
	// FILES/ too (e.g. when hand-editing static assets you want hot-reloaded).
	IncludeFiles bool
}

// WatchOps watches dir recursively for changes to *.txcl and *.json
// files. Calls onChange with a debounced cadence — bursty editor
// saves coalesce into a single re-apply rather than firing N times.
//
// Blocks until ctx is canceled. Returns the watcher's exit error (or
// nil on clean shutdown).
func WatchOps(ctx context.Context, dir string, opts Options, onChange func()) error {
	return watchDir(ctx, dir, opts, relevantExt, onChange)
}

// WatchColocatedComputes watches the OPS/ tree for colocated compute source
// changes (.js/.ts) and fires onChange (debounced). Only compute source — not
// .txcl/.json, which the OPS draft watcher handles — so a compute edit
// triggers a rebuild+activate without double-firing on resonator/mock edits.
func WatchColocatedComputes(ctx context.Context, dir string, opts Options, onChange func()) error {
	return watchDir(ctx, dir, opts, isComputeSource, onChange)
}

// watchDir is the shared recursive-watch loop. match decides which changed
// paths fire onChange.
func watchDir(ctx context.Context, dir string, opts Options, match func(string) bool, onChange func()) error {
	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = 500 * time.Millisecond
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	w := watcher.New()
	w.SetMaxEvents(0)
	defer w.Close()

	// radovskyb/watcher is a POLLING watcher: every cycle it filepath.Walks
	// (lstats) the whole tree. Pruning a directory makes that walk SkipDir it —
	// the only way to keep tens of thousands of built-asset files from being
	// lstat'd 10×/sec. (A file-level filter hook doesn't help: a skipped file
	// is still walked.) Ignore the matched dirs BEFORE AddRecursive so the
	// initial walk skips them too.
	ignoreDirs, err := pruneDirs(absDir, opts.Ignore, opts.IncludeFiles)
	if err != nil {
		return err
	}
	if len(ignoreDirs) > 0 {
		if err := w.Ignore(ignoreDirs...); err != nil {
			return err
		}
	}

	if err := w.AddRecursive(absDir); err != nil {
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

// filesDir is the reserved per-stack asset directory. It holds built assets
// (served by txco://static), never editable source, so the dev watchers prune
// it by default — see Options.IncludeFiles.
const filesDir = "FILES"

// pruneDirs returns the directories under root whose subtrees should be
// excluded from the watch: any dir matching an Ignore glob, plus every FILES/
// asset tree unless includeFiles is set. A matched directory is recorded and
// not descended into.
func pruneDirs(root string, globs []string, includeFiles bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // tolerate transient races / permission errors; just don't prune
		}
		if !d.IsDir() || p == root {
			return nil
		}
		base := d.Name()
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if (!includeFiles && base == filesDir) || matchAnyGlob(globs, rel, base) {
			out = append(out, p)
			return filepath.SkipDir
		}
		return nil
	})
	return out, err
}

// matchAnyGlob reports whether rel (a slash path relative to the watch root) or
// base (the directory's name) matches any of the patterns. A pattern with a "/"
// matches the relative path; a bare pattern matches the base name anywhere.
func matchAnyGlob(globs []string, rel, base string) bool {
	for _, g := range globs {
		g = strings.TrimSuffix(strings.TrimSpace(filepath.ToSlash(g)), "/")
		if g == "" {
			continue
		}
		if strings.Contains(g, "/") {
			if ok, _ := path.Match(g, rel); ok {
				return true
			}
			continue
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
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
