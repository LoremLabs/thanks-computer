package dev

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchOpsDebounces verifies that bursty saves coalesce into one
// onChange call. Five touches inside a 200ms window debounced at 250ms
// should yield exactly one event.
func TestWatchOpsDebounces(t *testing.T) {
	dir := t.TempDir()
	// Pre-create a file the watcher will see updates on.
	target := filepath.Join(dir, "rule.txcl")
	if err := os.WriteFile(target, []byte("v0"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var fired int32
	go func() {
		_ = WatchOps(ctx, dir, Options{Debounce: 250 * time.Millisecond}, func() {
			atomic.AddInt32(&fired, 1)
		})
	}()
	// Give the watcher a beat to start polling (it has a 100ms tick).
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 5; i++ {
		_ = os.WriteFile(target, []byte{byte('0' + i)}, 0o644)
		time.Sleep(40 * time.Millisecond)
	}
	// Wait long enough for the debounce window to close + extra slack.
	time.Sleep(500 * time.Millisecond)

	got := atomic.LoadInt32(&fired)
	if got < 1 {
		t.Errorf("got %d fires, want at least 1", got)
	}
	if got > 2 {
		// 1 is ideal; 2 can happen on macOS where the polling watcher
		// catches an in-flight write across two ticks. Anything more
		// means debounce is broken.
		t.Errorf("got %d fires; debounce should keep this <= 2", got)
	}
}

// TestWatchOpsIgnoresUnrelated: a non-.txcl/.json file change must NOT
// fire onChange.
func TestWatchOpsIgnoresUnrelated(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("v0"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var fired int32
	go func() {
		_ = WatchOps(ctx, dir, Options{Debounce: 200 * time.Millisecond}, func() {
			atomic.AddInt32(&fired, 1)
		})
	}()
	time.Sleep(200 * time.Millisecond)

	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte{byte('0' + i)}, 0o644)
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Errorf("got %d fires for README.md changes; want 0", got)
	}
}

// TestPruneDirs checks the subtree-pruning rules: FILES/ is excluded by
// default, kept with IncludeFiles, and custom globs match by base name or
// relative path. A matched directory is reported once and not descended into.
func TestPruneDirs(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"www/1000/op",
		"www/FILES/app/immutable",
		"publications/white-fang/FILES",
		"publications/white-fang/1000",
		"node_modules/pkg",
	} {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	abs := func(p string) string { return filepath.Join(root, p) }
	assertSet := func(name string, got []string, want ...string) {
		t.Helper()
		set := map[string]bool{}
		for _, g := range got {
			set[g] = true
		}
		if len(set) != len(want) {
			t.Fatalf("%s: pruned %v, want %v", name, got, want)
		}
		for _, w := range want {
			if !set[w] {
				t.Fatalf("%s: pruned %v, missing %s", name, got, w)
			}
		}
	}

	got, err := pruneDirs(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	assertSet("default", got, abs("www/FILES"), abs("publications/white-fang/FILES"))

	got, _ = pruneDirs(root, nil, true)
	assertSet("includeFiles", got)

	got, _ = pruneDirs(root, []string{"node_modules"}, true)
	assertSet("bare-name glob", got, abs("node_modules"))

	got, _ = pruneDirs(root, []string{"publications/*/FILES"}, true)
	assertSet("path glob", got, abs("publications/white-fang/FILES"))
}

// TestWatchExcludesFilesByDefault: a change to a FILES/ asset (even a .json,
// which the OPS watcher otherwise acts on) must NOT fire by default, while a
// real source edit still does.
func TestWatchExcludesFilesByDefault(t *testing.T) {
	dir := t.TempDir()
	filesJSON := filepath.Join(dir, "FILES", "app", "version.json")
	if err := os.MkdirAll(filepath.Dir(filesJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filesJSON, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rule.txcl"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var fired int32
	go func() {
		_ = WatchOps(ctx, dir, Options{Debounce: 200 * time.Millisecond}, func() { atomic.AddInt32(&fired, 1) })
	}()
	time.Sleep(250 * time.Millisecond)

	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filesJSON, []byte{byte('0' + i)}, 0o644)
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("FILES/ change fired %d times; want 0 (excluded by default)", got)
	}

	_ = os.WriteFile(filepath.Join(dir, "rule.txcl"), []byte("v1"), 0o644)
	time.Sleep(500 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got < 1 {
		t.Fatalf("source edit fired %d; want >= 1", got)
	}
}

func TestIsComputeSource(t *testing.T) {
	cases := map[string]bool{
		"OPS/site/100/hello.js": true,
		"handler.ts":            true,
		"mod.mjs":               true,
		"hello.txcl":            false, // rules handled by the draft watcher
		"mock-request.json":     false, // mocks handled by the draft watcher
		"README.md":             false,
	}
	for p, want := range cases {
		if got := isComputeSource(p); got != want {
			t.Errorf("isComputeSource(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestWatchColocatedComputes: editing a colocated .js fires; a .txcl/.json
// (rule/mock — handled by the OPS draft watcher) does NOT, so a compute edit
// doesn't double-fire on rule/mock saves.
func TestWatchColocatedComputes(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "OPS/site/100/hello.js")
	if err := os.MkdirAll(filepath.Dir(js), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(js, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "OPS/site/100/hello.txcl"), []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var fired int32
	go func() {
		_ = WatchColocatedComputes(ctx, dir, Options{Debounce: 250 * time.Millisecond}, func() { atomic.AddInt32(&fired, 1) })
	}()
	time.Sleep(200 * time.Millisecond)

	// .txcl edits must NOT fire the compute watcher.
	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filepath.Join(dir, "OPS/site/100/hello.txcl"), []byte{byte('0' + i)}, 0o644)
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got != 0 {
		t.Fatalf("got %d fires for .txcl writes; want 0", got)
	}

	// A .js edit fires.
	_ = os.WriteFile(js, []byte("v1"), 0o644)
	time.Sleep(500 * time.Millisecond)
	if got := atomic.LoadInt32(&fired); got < 1 {
		t.Fatalf("got %d fires for .js edit; want >= 1", got)
	}
}
