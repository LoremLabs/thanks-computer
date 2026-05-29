package signer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// FileKeySigner signs RFC 9421 requests with an Ed25519 private key
// loaded from a PEM file. The reader transparently handles both the
// legacy PKCS#8 form txco shipped originally AND the OpenSSH form
// `ssh-keygen` produces, via crypto/ssh.ParseRawPrivateKey. Existing
// developer keys keep working; new keys can come straight from a
// standard SSH workflow.
type FileKeySigner struct {
	keyID string
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
}

// NewFileKeySigner loads path and returns a signer. Encrypted-key
// behavior depends on promptPassphrase:
//   - true + TTY: prompts the user via term.ReadPassword.
//   - true + non-TTY (pipe): returns *PassphraseMissingError so the
//     CLI can surface a clear "this key is encrypted; pass it via
//     --passphrase or run interactively" error.
//   - false: never prompts; *PassphraseMissingError straight away.
func NewFileKeySigner(keyID, path string, promptPassphrase bool) (*FileKeySigner, error) {
	if keyID == "" {
		return nil, errors.New("file signer: empty keyID")
	}
	priv, err := LoadEd25519PrivateKey(path, promptPassphrase)
	if err != nil {
		return nil, err
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("file signer: loaded key from %q is not ed25519 (got %T)", path, priv.Public())
	}
	return &FileKeySigner{keyID: keyID, priv: priv, pub: pub}, nil
}

// NewFileKeySignerFromKey constructs a signer from an already-loaded
// private key. Useful for tests and for the enrollment path that
// generates a fresh key in-memory before persisting it.
func NewFileKeySignerFromKey(keyID string, priv ed25519.PrivateKey) (*FileKeySigner, error) {
	if keyID == "" {
		return nil, errors.New("file signer: empty keyID")
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("file signer: not an ed25519 key (got %T)", priv.Public())
	}
	return &FileKeySigner{keyID: keyID, priv: priv, pub: pub}, nil
}

// KeyID implements Signer.
func (s *FileKeySigner) KeyID() string { return s.keyID }

// PublicKey implements Signer.
func (s *FileKeySigner) PublicKey() ed25519.PublicKey { return s.pub }

// Sign implements Signer. Walks the standard canonicalize → sign →
// set-headers path with raw `ed25519.Sign`. Same wire output as the
// AgentSigner; only the signing primitive differs.
func (s *FileKeySigner) Sign(req *http.Request, body []byte) error {
	digest := computeContentDigest(req, body)
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("file signer: nonce: %w", err)
	}
	params := signParams{KeyID: s.keyID, Created: nowUnix(), Nonce: nonce}
	inputValue := buildSignatureInputValue(params)
	base := buildSignatureBase(req, digest, inputValue)

	sig := ed25519.Sign(s.priv, base)
	sigB64 := base64.StdEncoding.EncodeToString(sig)
	req.Header.Set("Signature-Input", signatureLabel+"="+inputValue)
	req.Header.Set("Signature", signatureLabel+"=:"+sigB64+":")
	return nil
}

// LoadEd25519PrivateKey reads path and decodes it to a raw
// ed25519.PrivateKey. Handles PKCS#8 PEM, OpenSSH PEM, and encrypted
// OpenSSH (with passphrase prompt when promptPassphrase is true and
// stdin is a TTY).
//
// Encrypted PKCS#8 is intentionally not supported — too rare to be
// worth the surface; users get a clear "re-encrypt with `ssh-keygen
// -p`" message via the underlying ssh.PassphraseMissingError path.
func LoadEd25519PrivateKey(path string, promptPassphrase bool) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	parsed, parseErr := ssh.ParseRawPrivateKey(raw)
	if parseErr != nil {
		var mpe *ssh.PassphraseMissingError
		if !errors.As(parseErr, &mpe) {
			return nil, fmt.Errorf("parse %q: %w", path, parseErr)
		}
		// Encrypted key path.
		if !promptPassphrase || !term.IsTerminal(int(os.Stdin.Fd())) {
			return nil, &PassphraseMissingError{Path: path}
		}
		pp, err := promptPassphraseFn(path)
		if err != nil {
			return nil, fmt.Errorf("read passphrase: %w", err)
		}
		parsed, err = ssh.ParseRawPrivateKeyWithPassphrase(raw, pp)
		if err != nil {
			return nil, fmt.Errorf("parse %q with passphrase: %w", path, err)
		}
	}
	return castEd25519(parsed, path)
}

// LoadEd25519PrivateKeyWithPassphrase decodes an encrypted PEM key
// using the supplied passphrase. Returns a clear error if path is
// not an encrypted key or the passphrase is wrong.
func LoadEd25519PrivateKeyWithPassphrase(path string, passphrase []byte) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	parsed, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, passphrase)
	if err != nil {
		return nil, fmt.Errorf("parse %q with passphrase: %w", path, err)
	}
	return castEd25519(parsed, path)
}

// castEd25519 normalizes the various concrete types ssh.ParseRawPrivateKey
// can return for an Ed25519 key into a single ed25519.PrivateKey value.
// Some Go versions return a pointer, some a value — handle both.
func castEd25519(parsed any, path string) (ed25519.PrivateKey, error) {
	switch k := parsed.(type) {
	case ed25519.PrivateKey:
		return k, nil
	case *ed25519.PrivateKey:
		return *k, nil
	default:
		return nil, fmt.Errorf("file %q is not an ed25519 key (got %T)", path, parsed)
	}
}

// PassphraseMissingError is the typed sentinel callers use to detect
// "this key is encrypted, you need to supply a passphrase." Mirrors
// ssh.PassphraseMissingError but is owned by this package so callers
// don't have to import x/crypto/ssh just for type assertions.
type PassphraseMissingError struct{ Path string }

// Error implements error.
func (e *PassphraseMissingError) Error() string {
	return fmt.Sprintf("key file %q is passphrase-protected; pass a passphrase or run interactively to be prompted", e.Path)
}

// promptPassphraseFn is the seam tests use to inject a synthetic
// passphrase without touching a real terminal. Production reads from
// os.Stdin via term.ReadPassword.
var promptPassphraseFn = func(path string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "passphrase for %s: ", path)
	pp, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	return pp, nil
}

// Fingerprint returns the SHA256 fingerprint of an Ed25519 public
// key in the canonical ssh-keygen `-lf` format
// (`SHA256:<base64-no-pad>`). Computes over the SSH wire format, not
// the raw 32 bytes, so the value matches `ssh-add -l` / `ssh-keygen
// -lf` output verbatim — letting users cross-reference txco's choice
// against what their ssh tooling shows.
//
// Falls back to a raw-key SHA256 (still useful as a stable identifier)
// only if ssh.NewPublicKey somehow fails, which can't happen for a
// well-formed 32-byte ed25519 key but is handled defensively.
func Fingerprint(pub ed25519.PublicKey) string {
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		sum := sha256.Sum256(pub)
		return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	}
	return ssh.FingerprintSHA256(sshPub)
}
