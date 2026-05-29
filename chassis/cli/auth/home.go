// Package auth implements the `txco auth …` CLI surface — keygen, dev
// enrollment, signed whoami, key rotation, and revocation. It's the
// client-side counterpart to chassis/server/admin's auth endpoints.
//
// All persistent state (keys + meta files) lives under TXCO_HOME,
// defaulting to ~/.config/txco/.
package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HomePath returns the directory where keys + meta files live. Honors
// TXCO_HOME; otherwise $XDG_CONFIG_HOME/txco; otherwise ~/.config/txco.
// The directory is created with 0700 mode on first call so sibling
// files (private keys) can default to 0600 without surprise.
func HomePath() (string, error) {
	if v := os.Getenv("TXCO_HOME"); v != "" {
		if err := os.MkdirAll(v, 0o700); err != nil {
			return "", fmt.Errorf("auth: mkdir TXCO_HOME %q: %w", v, err)
		}
		return v, nil
	}
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		dir := filepath.Join(v, "txco")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("auth: mkdir %q: %w", dir, err)
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("auth: resolve home: %w", err)
	}
	dir := filepath.Join(home, ".config", "txco")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("auth: mkdir %q: %w", dir, err)
	}
	return dir, nil
}

// KeyPath returns the path to a named ed25519 key under TXCO_HOME. The
// keys/ subdirectory is created on demand; the key file itself isn't
// touched. Names should be bare (no `.ed25519` suffix); a name like
// "local" maps to `$TXCO_HOME/keys/local.ed25519`.
func KeyPath(name string) (string, error) {
	if name == "" {
		name = "local"
	}
	home, err := HomePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("auth: mkdir %q: %w", dir, err)
	}
	return filepath.Join(dir, name+".ed25519"), nil
}

// MetaPath returns the sibling meta-file path for a named key. The
// meta file lives next to the private key so a single rm cleans up
// both halves.
func MetaPath(name string) (string, error) {
	keyPath, err := KeyPath(name)
	if err != nil {
		return "", err
	}
	return keyPath + ".meta.json", nil
}

// HomePathPretty returns the home directory in user-readable form
// for printing in help text, flag descriptions, and prompts. It's
// the resolved path with $HOME contracted to "~", so users see
// "~/.config/txco/keys" rather than "$TXCO_HOME/keys" (which most
// people read as a placeholder) or
// "/Users/mattmankins/.config/txco/keys" (which differs per
// machine and is ugly in shared docs).
//
// Unlike HomePath, this does NOT create the directory — it's a
// pure path-prediction function safe to call from flag-definition
// time. Cached because help text is constructed many times.
func HomePathPretty() string {
	prettyHomeOnce.Do(func() {
		raw := ""
		if v := os.Getenv("TXCO_HOME"); v != "" {
			raw = v
		} else if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
			raw = filepath.Join(v, "txco")
		} else if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, ".config", "txco")
		} else {
			prettyHome = "~/.config/txco"
			return
		}
		// Contract $HOME → ~ so the output works for shared docs.
		if home, err := os.UserHomeDir(); err == nil &&
			strings.HasPrefix(raw, home+string(os.PathSeparator)) {
			prettyHome = "~" + raw[len(home):]
			return
		}
		prettyHome = raw
	})
	return prettyHome
}

var (
	prettyHome     string
	prettyHomeOnce sync.Once
)
