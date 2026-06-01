package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FileMasterKey is the bundled MasterKeyProvider: a 32-byte master
// key loaded from a host-local file. The file must have 0600 perms
// (no group/other bits set) and contain exactly masterKeySize bytes.
//
// The key is read once at construction and held in memory. The file
// is not re-read during the process lifetime — operators rotate the
// master key by restarting the chassis with a new file (multi-version
// online rotation is Phase 2; see internal docs/todo-secret-store.md §3).
type FileMasterKey struct {
	key     [masterKeySize]byte
	version int
}

// ErrMasterKeyMissing is returned when the configured master-key
// file does not exist. Distinguishable from malformed-file errors
// so chassis-boot logic can choose to log-and-continue rather than
// fail loud (the secret store is opt-in; missing key = feature off).
var ErrMasterKeyMissing = errors.New("secrets: master key file does not exist")

// ErrMasterKeyMalformed is returned when the file exists but its
// perms or contents are invalid.
var ErrMasterKeyMalformed = errors.New("secrets: master key file is malformed")

// NewFileMasterKey reads the master key from path.
//
// Errors:
//   - wraps ErrMasterKeyMissing if the file does not exist
//   - wraps ErrMasterKeyMalformed if perms are not 0600 or size != 32
//   - wraps the underlying os.Stat / read error for any other failure
//
// On success, the file's bytes are copied into the returned
// FileMasterKey and the original buffer is zeroed.
func NewFileMasterKey(path string) (*FileMasterKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrMasterKeyMissing, path)
		}
		return nil, fmt.Errorf("secrets: stat master key %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q is not a regular file", ErrMasterKeyMalformed, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: %q must have 0600 perms, got %o", ErrMasterKeyMalformed, path, info.Mode().Perm())
	}
	if info.Size() != masterKeySize {
		return nil, fmt.Errorf("%w: %q must be exactly %d bytes, got %d", ErrMasterKeyMalformed, path, masterKeySize, info.Size())
	}

	buf := make([]byte, masterKeySize)
	defer Zero(buf)

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: open master key %q: %w", path, err)
	}
	defer f.Close()

	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("secrets: read master key %q: %w", path, err)
	}

	mk := &FileMasterKey{version: 1}
	copy(mk.key[:], buf)
	return mk, nil
}

// Key returns the master key. The returned slice aliases internal
// storage; callers must not mutate. (Per MasterKeyProvider contract.)
func (m *FileMasterKey) Key() []byte { return m.key[:] }

// Version returns the master-key generation. Always 1 for
// FileMasterKey in v1; multi-version is Phase 2.
func (m *FileMasterKey) Version() int { return m.version }

// LoadOrMintFileMasterKey is the chassis-boot-time entry point.
// Loads the master key from path if the file exists and is valid;
// auto-mints a fresh key at path if the file is missing.
//
// On first-mint, calls notifyMint(path) so the caller can log the
// event prominently — auto-creating a key carries a real operator
// obligation (back this up; losing it makes every stored secret
// unrecoverable). Pass nil to skip the notification.
//
// Returns any wrapped error other than ErrMasterKeyMissing
// (malformed file, bad perms, mint failure, etc.). Callers treat
// non-nil error as "feature off; log it and continue booting".
//
// Mirrors the runtime DB lifecycle: the chassis creates what it
// needs on first run; operators override the path via config when
// they want artifacts somewhere else.
func LoadOrMintFileMasterKey(path string, notifyMint func(path string)) (*FileMasterKey, error) {
	mk, err := NewFileMasterKey(path)
	if err == nil {
		return mk, nil
	}
	if !errors.Is(err, ErrMasterKeyMissing) {
		return nil, err
	}
	// Missing ⇒ auto-mint.
	if mintErr := MintFileMasterKey(path); mintErr != nil {
		return nil, fmt.Errorf("auto-mint: %w", mintErr)
	}
	if notifyMint != nil {
		notifyMint(path)
	}
	return NewFileMasterKey(path)
}

// MintFileMasterKey writes 32 fresh random bytes to path with 0600
// perms. Refuses to overwrite an existing file (O_EXCL); the caller
// is responsible for any "do you want to overwrite" UX before
// removing an existing file.
//
// Used by `txco auth secrets init` (explicit), `txco dev`'s
// auto-mint (dev workdir), and LoadOrMintFileMasterKey (chassis
// boot). Same primitive, multiple call sites, one implementation.
func MintFileMasterKey(path string) error {
	// Ensure the parent directory exists (the user may pass a path
	// under a fresh directory like /data/secrets/).
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("secrets: create parent dir for %q: %w", path, err)
		}
	}

	key := make([]byte, masterKeySize)
	defer Zero(key)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return fmt.Errorf("secrets: mint master key: %w", err)
	}

	// O_EXCL + 0600 — refuses to overwrite, atomic create.
	// Precedent: chassis/cli/auth/keys.go:67.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("secrets: write master key %q: %w", path, err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // partial write — don't leave a corrupt file behind
		return fmt.Errorf("secrets: write master key %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets: close master key %q: %w", path, err)
	}
	return nil
}
