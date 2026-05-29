package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile model — AWS / gcloud style. A "profile" is a named meta
// file under $TXCO_HOME/keys/<name>.meta.json. The meta already
// carries everything a profile needs: chassis_url, actor_id, key_id,
// key_source, optional key_path. We never duplicate that data.
//
// Selection precedence at sign time (highest first):
//
//	1. --profile <name>           explicit flag on the command
//	2. TXCO_PROFILE env var       per-shell override
//	3. $TXCO_HOME/active file     the persisted selection
//	4. "local"                    historical default (back-compat)
//
// TXCO_PRIVATE_KEY_PATH still wins above all four — that's the
// escape hatch for tests and very-explicit CI configs.
//
// Sentinel ActiveNone means "no profile active" (the user ran
// `txco auth logout`). When this is the resolved selection,
// commands send unsigned rather than picking the "local" fallback.

// DefaultProfile is the historical default name. When no $TXCO_HOME/active
// file exists, this is what commands fall back to — so existing
// developers running `txco auth bootstrap-local` then `txco apply`
// keep working with zero migration.
const DefaultProfile = "local"

// ActiveNone is the sentinel written to $TXCO_HOME/active by
// `txco auth logout`. When the resolved profile is this string,
// callers MUST NOT try to sign — they send the request unsigned and
// let the chassis decide (e.g. auth-mode=both accepts it).
const ActiveNone = "none"

// activeFileName is the basename of the active-profile pointer.
// Lives next to keys/ rather than inside it so a `keys/*.ed25519`
// glob can't accidentally include it.
const activeFileName = "active"

// activePath returns $TXCO_HOME/active. Honors TXCO_HOME via
// HomePath, so tests that swap TXCO_HOME via t.Setenv work without
// special-casing.
func activePath() (string, error) {
	home, err := HomePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, activeFileName), nil
}

// ReadActiveProfile returns the contents of $TXCO_HOME/active, or
// DefaultProfile when the file is missing. Returns ActiveNone when
// the file explicitly says "none" (the logout state).
//
// Whitespace + trailing newline are trimmed so an editor that
// helpfully adds a final newline doesn't break selection.
func ReadActiveProfile() (string, error) {
	p, err := activePath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultProfile, nil
		}
		return "", fmt.Errorf("read active profile %q: %w", p, err)
	}
	name := strings.TrimSpace(string(raw))
	if name == "" {
		return DefaultProfile, nil
	}
	return name, nil
}

// WriteActiveProfile persists name as the active profile. Use
// ActiveNone to mark "logged out" (callers won't try to sign).
// Atomic-ish: write to a temp file then rename, so a crash mid-
// write can't leave a half-written active pointer that breaks
// every command.
func WriteActiveProfile(name string) error {
	if name == "" {
		return errors.New("active profile name cannot be empty (use ActiveNone for logout)")
	}
	if !validKeyName(name) && name != ActiveNone {
		return fmt.Errorf("invalid profile name %q (use letters, digits, '_' or '-')", name)
	}
	p, err := activePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(p), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".active.*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write active: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close active: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("chmod active: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename active: %w", err)
	}
	return nil
}

// ResolveProfile walks the precedence chain (flag → env → active
// file → default) and returns the profile name to use. Empty flag
// + empty env means "let the persisted state decide"; an explicit
// flag short-circuits everything.
//
// The TXCO_PRIVATE_KEY_PATH escape hatch is NOT handled here —
// it's resolved at a higher layer in target.go::loadSigner. This
// function is purely about the profile-name pick.
func ResolveProfile(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env := os.Getenv("TXCO_PROFILE"); env != "" {
		return env, nil
	}
	return ReadActiveProfile()
}

// ProfileChassisURL returns the chassis_url recorded in the resolved
// signing profile's meta (what `txco auth bootstrap-local` / `accept`
// bound it to), or "" when logged out, no profile/meta exists, or no
// URL was recorded. Lets endpoint-less commands fall back to the
// profile's own chassis instead of a blind localhost default.
func ProfileChassisURL(profileFlag string) string {
	name, err := ResolveProfile(profileFlag)
	if err != nil || name == "" || name == ActiveNone {
		return ""
	}
	metaPath, err := MetaPath(name)
	if err != nil {
		return ""
	}
	m, err := LoadMeta(metaPath)
	if err != nil || m == nil {
		return ""
	}
	return strings.TrimSpace(m.ChassisURL)
}

// ProfileInfo is the summary `txco auth profiles` renders. The
// meta is loaded on demand by ListProfiles for each candidate, so
// callers get fully-populated rows in one shot.
type ProfileInfo struct {
	Name   string
	Active bool
	Meta   *Meta
}

// ListProfiles enumerates every <name>.meta.json under
// $TXCO_HOME/keys/ and tags whichever one is currently active.
// Sort order: active first, then alphabetical — so `txco auth
// profiles` immediately shows what's in play.
func ListProfiles() ([]ProfileInfo, error) {
	home, err := HomePath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "keys")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %q: %w", dir, err)
	}
	active, err := ReadActiveProfile()
	if err != nil {
		// Don't fail enumeration just because the active pointer
		// is unreadable; fall back to DefaultProfile.
		active = DefaultProfile
	}
	const metaSuffix = ".ed25519.meta.json"
	var out []ProfileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, metaSuffix) {
			continue
		}
		base := strings.TrimSuffix(name, metaSuffix)
		mp := filepath.Join(dir, name)
		m, err := LoadMeta(mp)
		if err != nil {
			// Skip unreadable meta files rather than refusing the
			// whole listing — the user can still see other
			// profiles and remove the broken one explicitly.
			continue
		}
		out = append(out, ProfileInfo{
			Name:   base,
			Active: active != ActiveNone && active == base,
			Meta:   m,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
