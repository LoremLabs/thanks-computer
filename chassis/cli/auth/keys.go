package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// GenerateKey produces a fresh ed25519 keypair. Wraps the stdlib so
// callers don't all need to import crypto/ed25519 directly.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// SavePrivateKey writes priv to path in OpenSSH PEM format
// (`OPENSSH PRIVATE KEY`) AND a matching `<path>.pub` sidecar in
// authorized_keys format. This is the same on-disk shape
// `ssh-keygen -t ed25519 -f <path>` produces, which means:
//
//   - Users can inspect / move / passphrase-encrypt the file with
//     standard SSH tooling (`ssh-keygen -p -f <path>`).
//   - The file is interchangeable with `~/.ssh/id_ed25519` and the
//     signer package's FileKeySigner reads it transparently.
//   - Other SSH tools (`ssh-add`, `ssh-copy-id`, `ssh -i <path>`)
//     all work on the file because it has the canonical `.pub`
//     sidecar they look for.
//
// Existing developer keys written by older txco versions (PKCS#8
// `PRIVATE KEY` blocks) are NOT rewritten — the reader handles both
// formats, so they keep working until the user rotates voluntarily.
//
// File mode is 0600 for the private key, 0644 for the public
// sidecar. Refuses to overwrite an existing file — rotation is
// explicit (delete old, then save new) so a stray `auth init` can
// never silently clobber a working key.
func SavePrivateKey(path string, priv ed25519.PrivateKey) error {
	return SavePrivateKeyWithComment(path, priv, "")
}

// SavePrivateKeyWithComment is the labeled variant of SavePrivateKey.
// `comment` is written as the third whitespace-separated field of
// the `.pub` sidecar (the same place `ssh-keygen -C "matt@laptop"`
// puts it). Empty comment yields a bare two-field line, exactly
// what `ssh-keygen` without `-C` produces.
func SavePrivateKeyWithComment(path string, priv ed25519.PrivateKey, comment string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("auth: refusing to overwrite existing key %q", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth: stat %q: %w", path, err)
	}

	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return fmt.Errorf("auth: marshal openssh: %w", err)
	}
	pemBytes := pem.EncodeToMemory(block)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("auth: create %q: %w", path, err)
	}
	if _, err := f.Write(pemBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("auth: write %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("auth: close %q: %w", path, err)
	}

	// Write the .pub sidecar so the file looks like a standard
	// ssh-keygen output. We tolerate sidecar failures by NOT
	// removing the private key — the user can write the sidecar
	// later with `ssh-keygen -y -f <path> > <path>.pub` if needed.
	// Print a warning so they know.
	if err := writePubSidecar(path, priv.Public().(ed25519.PublicKey), comment); err != nil {
		return fmt.Errorf("auth: write %q.pub: %w", path, err)
	}
	return nil
}

// writePubSidecar produces "<path>.pub" in the standard
// authorized_keys format: `ssh-ed25519 <base64-blob> [comment]\n`.
// 0644 mode — public keys are readable, like ssh-keygen produces.
func writePubSidecar(privPath string, pub ed25519.PublicKey, comment string) error {
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")
	if comment != "" {
		line += " " + comment
	}
	line += "\n"
	return os.WriteFile(privPath+".pub", []byte(line), 0o644)
}

// LoadPrivateKey reads an Ed25519 private key from path. Accepts
// both PKCS#8 PEM (the legacy txco format) and OpenSSH PEM (the new
// default, and the format `ssh-keygen` produces). Encrypted keys
// trigger a passphrase prompt on TTY; non-TTY callers get a typed
// signer.PassphraseMissingError so they can surface a clear error.
//
// Delegates to the signer package so there's exactly one parser
// shared with FileKeySigner — no risk of the two diverging.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	return signer.LoadEd25519PrivateKey(path, true)
}

// LoadPrivateKeyWithPassphrase decrypts an encrypted Ed25519 key
// using the supplied passphrase. For non-interactive callers (CI,
// scripts) that already have the passphrase in hand.
func LoadPrivateKeyWithPassphrase(path string, passphrase []byte) (ed25519.PrivateKey, error) {
	return signer.LoadEd25519PrivateKeyWithPassphrase(path, passphrase)
}

// PublicKeyB64 returns the base64-encoded (standard, padded) form of
// the public key. This is what `/auth/dev/enroll` expects in the
// `public_key_b64` field.
func PublicKeyB64(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}
