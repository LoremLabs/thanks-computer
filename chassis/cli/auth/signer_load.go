package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// LoadSignerForActiveProfile is the profile-aware sign-time entry.
// Walks the precedence chain (flag → TXCO_PROFILE → $TXCO_HOME/active
// → DefaultProfile) and returns a Signer for the resolved profile.
//
// Returns (nil, nil) — note: nil signer, nil error — for two cases:
//   - Resolved profile is ActiveNone (`txco auth logout`). The
//     caller should send the request unsigned.
//   - The resolved profile has no meta file. Same idea: nothing
//     configured, send unsigned.
//
// Any other error (unreadable meta, bad backend, etc.) bubbles up.
func LoadSignerForActiveProfile(flag string) (signer.Signer, error) {
	name, err := ResolveProfile(flag)
	if err != nil {
		return nil, err
	}
	if name == ActiveNone {
		return nil, nil
	}
	return LoadSignerForName(name)
}

// LoadSignerForName resolves a signer.Signer for the meta file at
// $TXCO_HOME/keys/<name>.meta.json. Dispatches on meta.KeySource:
// file-backed metas yield a FileKeySigner; ssh-agent metas yield an
// AgentSigner. Legacy metas (no KeySource field) are treated as
// file-backed at the canonical $TXCO_HOME/keys/<name>.ed25519 path.
//
// Returns (nil, nil) — note: nil error, nil signer — when no meta
// file exists for that name (the common "no signing key configured"
// case). All other failure modes return a typed error so callers can
// surface a clear message rather than silently fall through.
func LoadSignerForName(name string) (signer.Signer, error) {
	if name == "" {
		name = defaultKeyName
	}
	metaPath, err := MetaPath(name)
	if err != nil {
		return nil, fmt.Errorf("resolve meta path: %w", err)
	}
	return LoadSignerForMetaPath(metaPath)
}

// LoadSignerForMetaPath is the explicit-path variant of
// LoadSignerForName — used when the caller already has an absolute
// meta path (e.g. derived from $TXCO_PRIVATE_KEY_PATH + ".meta.json").
//
// Same nil-signer-no-error semantics for the "meta file doesn't
// exist" case.
func LoadSignerForMetaPath(metaPath string) (signer.Signer, error) {
	m, err := LoadMeta(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load meta %q: %w", metaPath, err)
	}
	if m.KeyID == "" {
		return nil, fmt.Errorf("meta %q has no key_id; re-run `txco auth bootstrap-local` or `txco auth accept`", metaPath)
	}

	switch m.EffectiveKeySource() {
	case SourceSSHAgent:
		pub, err := decodeMetaPub(m.PublicKeyB64)
		if err != nil {
			return nil, fmt.Errorf("meta %q: %w", metaPath, err)
		}
		s, err := signer.NewAgentSigner(m.KeyID, pub)
		if err != nil {
			return nil, fmt.Errorf("ssh-agent backend for %q: %w", metaPath, err)
		}
		return s, nil

	case SourceFile:
		fallthrough
	default:
		kp := m.KeyPath
		if kp == "" {
			// Legacy meta files (pre-pluggable) and meta files with
			// the default key location: derive the key path from
			// the meta path by stripping ".meta.json".
			kp = stripMetaExt(metaPath)
		}
		if _, statErr := os.Stat(kp); statErr != nil {
			return nil, fmt.Errorf("key file %q from meta %q: %w", kp, metaPath, statErr)
		}
		// promptPassphrase=false: signed requests happen mid-pipeline
		// (e.g. `txco apply`) where an unexpected passphrase prompt
		// would block the script. Encrypted keys should live in
		// ssh-agent — the agent does the unlock once, and txco just
		// asks for signatures.
		s, err := signer.NewFileKeySigner(m.KeyID, kp, false)
		if err != nil {
			return nil, fmt.Errorf("file backend for %q: %w", metaPath, err)
		}
		return s, nil
	}
}

// stripMetaExt turns "…/local.ed25519.meta.json" back into
// "…/local.ed25519". Used to derive a default key path when meta
// doesn't carry one (legacy files).
func stripMetaExt(metaPath string) string {
	const suffix = ".meta.json"
	if len(metaPath) > len(suffix) && metaPath[len(metaPath)-len(suffix):] == suffix {
		return metaPath[:len(metaPath)-len(suffix)]
	}
	return metaPath
}

// decodeMetaPub accepts either std (padded) or url-safe base64 — the
// chassis's /auth/dev/enroll tolerates both, so we should too rather
// than failing on what's effectively the same key under a different
// encoding.
func decodeMetaPub(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, errors.New("public_key_b64 is empty")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("public_key_b64 %q does not decode to a 32-byte ed25519 key", s)
}
