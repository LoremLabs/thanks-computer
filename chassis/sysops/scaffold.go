package sysops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/loremlabs/thanks-computer/db"
)

// Scaffold materializes the embedded system bundle into a workspace so
// the `_sys/boot` router is visible and editable instead of an
// invisible binary default. destRoot is the workspace root; files land
// alongside application stacks in the same tree:
//
//	<destRoot>/OPS/_sys/boot/0/detect.txcl
//	<destRoot>/OPS/_sys/boot/20/route.txcl
//
// Because Load merges the on-disk bundle per (stack, scope, name) onto
// the embed, an unedited scaffold is behaviour-neutral; the operator
// can then edit boot/0, add a boot/10, etc.
//
// No-clobber is per file: an existing file is never overwritten unless
// force is set, so operator edits survive and only genuinely-new
// embedded files appear. The embed root is `opstacks/`; that prefix is
// stripped so the embedded `opstacks/OPS/...` lands at
// `<destRoot>/OPS/...`. Returns whether anything was written.
//
// Only `txco dev` calls this — `txco serve` stays static and uses the
// embed (or an explicitly configured --system-opstacks-dir) directly.
func Scaffold(destRoot string, force bool) (wrote bool, err error) {
	err = fs.WalkDir(db.SystemOpstacksFS, embedRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, embedRoot+"/") // e.g. OPS/_sys/boot/0/detect.txcl
		out := filepath.Join(destRoot, filepath.FromSlash(rel))
		if !force {
			if _, statErr := os.Stat(out); statErr == nil {
				return nil // keep operator edits.
			}
		}
		content, rErr := fs.ReadFile(db.SystemOpstacksFS, p)
		if rErr != nil {
			return fmt.Errorf("read embedded %s: %w", p, rErr)
		}
		if mkErr := os.MkdirAll(filepath.Dir(out), 0o755); mkErr != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(out), mkErr)
		}
		if wErr := os.WriteFile(out, content, 0o644); wErr != nil {
			return fmt.Errorf("write %s: %w", out, wErr)
		}
		wrote = true
		return nil
	})
	return wrote, err
}
