package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/webabi"
)

// devStacks is the set `txco dev` deploys: every stack with ops, plus each
// bound stack (a static build may have none), minus the bound stacks it
// can't deploy yet — a build that isn't there, or one with a server entry
// no chassis runs (unless staticOnly). Those are reported, not fatal: the
// dev loop keeps running and picks them up on the next rebuild.
func devStacks(ws *localWorkspace, ops []bundle.Op, staticOnly bool, stderr io.Writer) (map[string][]bundle.Op, map[string]bool) {
	stacks := groupOpsByStack(ops)
	allStacks := stackNames(ops)
	for n := range ws.Bindings {
		allStacks[n] = true
		if _, ok := stacks[n]; !ok {
			stacks[n] = nil
		}
	}
	for _, n := range sortedMapKeys(stacks) {
		if err := ws.Broken[n]; err != nil {
			fmt.Fprintf(stderr, "[txco] %s: skipped: %v\n", n, err)
			delete(stacks, n)
			continue
		}
		if d := ws.ABI[n]; d != nil && d.HasServer && !staticOnly {
			fmt.Fprintf(stderr, "[txco] %s: skipped: its Web ABI build has a server entry (%s), which no chassis runs yet; pass --static-only to deploy its static half\n",
				n, d.Manifest.Server.Entry)
			delete(stacks, n)
		}
	}
	return stacks, allStacks
}

// treeFingerprint is a stat-only fingerprint of every file under dir
// (path, size, mtime; no content read). Dot entries are skipped, as the
// installer skips them.
func treeFingerprint(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// abiBuildFingerprint is treeFingerprint for a Web ABI build, or "" while
// it has no manifest (not built yet, or mid-rebuild: producers wipe and
// rewrite the directory).
func abiBuildFingerprint(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, webabi.ManifestName)); err != nil {
		return ""
	}
	fp, err := treeFingerprint(dir)
	if err != nil {
		return ""
	}
	return fp
}

// watchABIBuild polls a Web ABI build every interval and calls onChange
// once a rebuild has settled: the build has a manifest, it differs from
// what was last seen, and it stayed the same for one more poll (a producer
// writes its output over several moments). The build as it stands at
// startup counts as seen. It returns when ctx ends.
func watchABIBuild(ctx context.Context, dir string, interval time.Duration, onChange func()) {
	last := abiBuildFingerprint(dir)
	pending := ""
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fp := abiBuildFingerprint(dir)
		switch {
		case fp == "" || fp == last:
			pending = ""
		case fp != pending:
			pending = fp // changed: give it one more poll to settle
		default:
			last, pending = fp, ""
			onChange()
		}
	}
}
