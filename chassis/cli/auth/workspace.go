package auth

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Workspace-scoped profile resolution: the git-style upward hunt.
//
// A workspace's txco.yaml already declares WHERE commands act (the
// `target:` → chassis binding); this file lets it also declare WHO acts
// there, via an optional `profile:` field on the target. Standing inside
// such a workspace, profile-following commands (`txco auth …`, kv, trace)
// resolve to the workspace's profile instead of the machine-global active
// one — which is what makes `txco auth tenant secrets set …` inside a dev
// workspace hit the dev chassis even while a production profile is the
// globally active one. The active profile stays global, persistent state;
// the workspace binding is committed, greppable provenance that narrows it.
//
// Deliberately minimal parse: only `target:` and `targets.<t>.profile` are
// read here (the full workspace schema lives in cli/target.go — this
// package cannot import it without a cycle, and needs nothing else).

// workspaceFileName is the marker the upward walk hunts for.
const workspaceFileName = "txco.yaml"

// workspaceWalkCap bounds the upward walk — belt and braces against
// filesystem-loop weirdness; no real tree is 64 directories deep.
const workspaceWalkCap = 64

type wsTarget struct {
	Profile string `yaml:"profile"`
}

type wsFile struct {
	Target  string              `yaml:"target"`
	Targets map[string]wsTarget `yaml:"targets"`
}

// FindWorkspaceConfig walks upward from dir looking for txco.yaml and
// returns its path, or "" when no workspace encloses dir.
func FindWorkspaceConfig(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for i := 0; i < workspaceWalkCap; i++ {
		p := filepath.Join(d, workspaceFileName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "" // hit the filesystem root
		}
		d = parent
	}
	return ""
}

// WorkspaceProfile resolves the enclosing workspace's profile binding.
//
// Returns ("", "", nil) — resolution falls through to the active
// profile — when there is no enclosing txco.yaml, when it has no default
// target, or when that target declares no `profile:`. A workspace that
// declares nothing keeps today's behavior exactly.
//
// Returns a non-nil error (LOUD, never a silent fallback) when the file
// is unreadable/unparseable or when it binds a profile that does not
// exist on this machine — silently falling back to the active profile
// there would re-create the exact wrong-chassis trap this exists to
// close, with extra steps.
func WorkspaceProfile() (name, source string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", nil // no cwd (deleted dir?) → no workspace opinion
	}
	path := FindWorkspaceConfig(cwd)
	if path == "" {
		return "", "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("workspace %s: %w", path, err)
	}
	var ws wsFile
	if err := yaml.Unmarshal(raw, &ws); err != nil {
		return "", "", fmt.Errorf("workspace %s: %w", path, err)
	}
	tname := ws.Target
	if tname == "" {
		tname = "dev" // the CLI-wide default target name
	}
	t, ok := ws.Targets[tname]
	if !ok || t.Profile == "" {
		return "", "", nil
	}

	// The binding must point at a real profile. `txco dev` registers a
	// keyless `dev` profile (meta only); enrolled profiles have meta +
	// key. Either artifact counts as existing.
	exists := false
	if mp, merr := MetaPath(t.Profile); merr == nil {
		if _, serr := os.Stat(mp); serr == nil {
			exists = true
		}
	}
	if !exists {
		if kp, kerr := KeyPath(t.Profile); kerr == nil {
			if _, serr := os.Stat(kp); serr == nil {
				exists = true
			}
		}
	}
	if !exists {
		return "", "", fmt.Errorf(
			"workspace %s binds target %q to profile %q, but no such profile exists on this machine\n"+
				"  (for a dev chassis: `txco dev` registers it; otherwise `txco auth accept` / `txco auth bootstrap-local`,\n"+
				"   or override with --profile / TXCO_PROFILE)",
			path, tname, t.Profile)
	}

	return t.Profile, fmt.Sprintf("%s target %q", path, tname), nil
}
