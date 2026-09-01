// Package state owns the .txco/<stack>.state.json file the txco CLI
// writes after a `pull` and reads on `push`. It pins which version of a
// stack a local workspace mirrors PER TARGET, so subsequent pushes set the
// right parent_version_number on the new draft and the fast-forward guard
// compares against the right chassis.
//
// State files are tiny JSON blobs at the root of the local workspace
// alongside the OPS/ directory:
//
//	./OPS/hello-world/...
//	./.txco/hello-world.state.json
//
// One file per stack, one entry per target key inside it
// ({"targets": {"<addr>|<tenant>": {...}}}). Before 2026-09-01 the file
// was a single flat entry with no record of WHICH chassis it mirrored, so
// every dev↔prod alternation read the other environment's number and the
// guard refused with a phantom rollback (onepony 0012 §7b trap 9). A
// legacy flat file reads as "no baseline" for every target and is
// rewritten in the keyed shape on the next Save — self-healing, one
// guard-blind window per stack.
//
// Nested stack names (e.g. "website/canary") use a hyphen-separated
// filename ("website-canary.state.json") so we don't have to create
// nested directories under .txco/. Workspace roots are mutable; we
// don't try to detect renames.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir is the workspace-relative directory that holds state files.
const Dir = ".txco"

// State is what the CLI persists per pulled stack.
type State struct {
	// VersionNumber is the per-stack number this workspace mirrors —
	// the same int users see in `txco versions <stack>` and in URLs.
	VersionNumber int64 `json:"version_number"`
	// ParentVersionNumber is what the next `push` will set as the new
	// draft's parent_version_number. After a sync it equals VersionNumber.
	// Both `pull` (fetch-based sync) and `push`/`apply` (which deploy the
	// local files and activate them) record a synced row with parent ==
	// version; `activate`/`draft` never write state (they don't establish
	// local==version).
	ParentVersionNumber int64 `json:"parent_version_number"`
	// ManifestHash is what the server reported at pull time. Used by
	// `txco push` to short-circuit "no changes locally" before walking
	// the tree (Phase 2; harmless to record now).
	ManifestHash string `json:"manifest_hash"`
}

// Key canonicalises a target identity (chassis addr + tenant slug) into
// the map key entries are stored under. Two spellings of one chassis
// (trailing slash, case in the host) must collide, or the same target
// grows two baselines.
func Key(addr, tenant string) string {
	a := strings.ToLower(strings.TrimRight(strings.TrimSpace(addr), "/"))
	return a + "|" + tenant
}

// stateFile is the on-disk shape: one entry per target key. A legacy flat
// file (a bare State, no "targets" key) decodes into a nil map.
type stateFile struct {
	Targets map[string]State `json:"targets"`
}

// fileFor returns the .txco/<safe>.state.json path under root.
func fileFor(root, stack string) string {
	safe := strings.ReplaceAll(stack, "/", "-")
	return filepath.Join(root, Dir, safe+".state.json")
}

// Load reads the state entry for `stack` under workspace `root`, for the
// target identified by `key` (see Key). Returns (nil, nil) when no entry
// exists for that target — callers default to "no parent" rather than
// failing, so a manual `txco push <stack>` against a hand-built OPS/ tree
// still works. A legacy flat file also reads as (nil, nil): its number is
// unattributable to any target, which is exactly the bug the keyed shape
// exists to fix.
func Load(root, stack, key string) (*State, error) {
	b, err := os.ReadFile(fileFor(root, stack))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("decode state %q: %w", stack, err)
	}
	if f.Targets == nil {
		return nil, nil // legacy flat file — no per-target baseline
	}
	s, ok := f.Targets[key]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

// Save writes the state entry for `key`, preserving other targets' entries
// and creating the .txco/ directory if needed. A legacy flat file is
// replaced wholesale (its one number belongs to no known target).
func Save(root, stack, key string, s State) error {
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return fmt.Errorf("mkdir .txco: %w", err)
	}
	var f stateFile
	if b, err := os.ReadFile(fileFor(root, stack)); err == nil {
		_ = json.Unmarshal(b, &f) // best-effort; legacy or corrupt → fresh map
	}
	if f.Targets == nil {
		f.Targets = map[string]State{}
	}
	f.Targets[key] = s
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(fileFor(root, stack), b, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}
