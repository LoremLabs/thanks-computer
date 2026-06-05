// Package state owns the .txco/<stack>.state.json file the txco CLI
// writes after a `pull` and reads on `push`. It pins which version of a
// stack a local workspace mirrors, so subsequent pushes set the right
// parent_version_number on the new draft.
//
// State files are tiny JSON blobs at the root of the local workspace
// alongside the OPS/ directory:
//
//	./OPS/hello-world/...
//	./.txco/hello-world.state.json
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

// fileFor returns the .txco/<safe>.state.json path under root.
func fileFor(root, stack string) string {
	safe := strings.ReplaceAll(stack, "/", "-")
	return filepath.Join(root, Dir, safe+".state.json")
}

// Load reads the state file for `stack` under workspace `root`. Returns
// (nil, nil) when no state exists yet — callers default to "no parent"
// rather than failing, so a manual `txco push <stack>` against a
// hand-built OPS/ tree still works.
func Load(root, stack string) (*State, error) {
	b, err := os.ReadFile(fileFor(root, stack))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode state %q: %w", stack, err)
	}
	return &s, nil
}

// Save writes the state file, creating the .txco/ directory if needed.
func Save(root, stack string, s State) error {
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return fmt.Errorf("mkdir .txco: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(fileFor(root, stack), b, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}
