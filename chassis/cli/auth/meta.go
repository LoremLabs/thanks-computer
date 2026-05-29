package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Source constants for Meta.KeySource. Kept as a small enum rather
// than an open string so a typo in a config-handler somewhere can't
// silently fall through to "file" (the default for old metas).
const (
	// SourceFile is the default: a private key on disk at KeyPath
	// (or at $TXCO_HOME/keys/<name>.ed25519 when KeyPath is empty).
	SourceFile = "file"
	// SourceSSHAgent: the key lives in ssh-agent and is identified
	// by PublicKeyB64. The local box never holds the private key.
	SourceSSHAgent = "ssh-agent"
)

// Meta is the JSON record persisted alongside a private key (or, for
// ssh-agent keys, in lieu of one). It remembers what the server told
// us at enrollment time so we don't have to re-derive `key_id` from
// the public key on every call, AND records how to find the matching
// signing material at request time.
type Meta struct {
	ActorID    string    `json:"actor_id"`
	KeyID      string    `json:"key_id"`
	ChassisURL string    `json:"chassis_url"`
	Label      string    `json:"label,omitempty"`
	EnrolledAt time.Time `json:"enrolled_at"`

	// KeySource selects the signing backend. Empty → SourceFile for
	// back-compat with pre-pluggable meta files (the old format
	// implicitly meant "file at the canonical location").
	KeySource string `json:"key_source,omitempty"`

	// PublicKeyB64 is the raw 32-byte ed25519 public key, base64
	// (std + padded). Populated for ssh-agent keys (the matcher
	// uses it) and for new file-backed keys (lets `txco auth
	// whoami` print a fingerprint without re-reading the key).
	// Optional for legacy meta files; falls back to deriving from
	// the on-disk key when needed.
	PublicKeyB64 string `json:"public_key_b64,omitempty"`

	// KeyPath is the absolute path to the private key file when
	// KeySource is SourceFile. Empty means "use the default
	// $TXCO_HOME/keys/<name>.ed25519" (back-compat). Useful for
	// pointing at ~/.ssh/id_ed25519 directly.
	KeyPath string `json:"key_path,omitempty"`

	// DefaultTenant is the chassis tenant slug this profile most
	// recently enrolled or accepted into. Used as the bottom rung of
	// the --tenant resolution precedence (flag → TXCO_TENANT env →
	// this field → "default"). Empty means "no preference; fall
	// through." Populated by bootstrap-local and accept once phase-3
	// memberships land; for back-compat with pre-tenant meta files
	// the loader treats empty as "default".
	DefaultTenant string `json:"default_tenant,omitempty"`
}

// EffectiveKeySource returns m.KeySource, defaulting to SourceFile
// for legacy meta files that predate the pluggable-backend change.
// Callers should use this instead of reading the field directly so
// "" doesn't appear as a "no backend" sentinel anywhere.
func (m *Meta) EffectiveKeySource() string {
	if m.KeySource == "" {
		return SourceFile
	}
	return m.KeySource
}

// SaveMeta writes meta to path with mode 0600. Overwrite is allowed —
// `enroll` rewrites this when re-enrolling against a new chassis.
func SaveMeta(path string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: marshal meta: %w", err)
	}
	// O_TRUNC because re-enrolling is a legitimate operation.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("auth: create meta %q: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("auth: write meta %q: %w", path, err)
	}
	return f.Close()
}

// LoadMeta reads a meta file. Returns (nil, os.ErrNotExist) — wrap
// with errors.Is to test for "no meta yet".
func LoadMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("auth: read meta %q: %w", path, err)
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("auth: parse meta %q: %w", path, err)
	}
	return &m, nil
}
