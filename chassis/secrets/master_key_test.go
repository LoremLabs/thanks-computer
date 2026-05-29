package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMintFileMasterKeyHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("MintFileMasterKey: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != masterKeySize {
		t.Errorf("size = %d, want %d", info.Size(), masterKeySize)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}
}

func TestMintFileMasterKeyRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	err := MintFileMasterKey(path)
	if err == nil {
		t.Fatalf("second mint should fail (O_EXCL)")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Errorf("expected os.ErrExist in chain, got: %v", err)
	}
}

func TestMintFileMasterKeyCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("MintFileMasterKey: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected key file at %s: %v", path, err)
	}
}

func TestNewFileMasterKeyHappy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("mint: %v", err)
	}

	mk, err := NewFileMasterKey(path)
	if err != nil {
		t.Fatalf("NewFileMasterKey: %v", err)
	}
	if mk.Version() != 1 {
		t.Errorf("Version = %d, want 1", mk.Version())
	}
	if len(mk.Key()) != masterKeySize {
		t.Errorf("Key size = %d, want %d", len(mk.Key()), masterKeySize)
	}
}

func TestNewFileMasterKeyMissing(t *testing.T) {
	_, err := NewFileMasterKey(filepath.Join(t.TempDir(), "does-not-exist.key"))
	if err == nil {
		t.Fatalf("expected error for missing file")
	}
	if !errors.Is(err, ErrMasterKeyMissing) {
		t.Errorf("expected ErrMasterKeyMissing in chain, got: %v", err)
	}
}

func TestNewFileMasterKeyRejectsBadPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := NewFileMasterKey(path)
	if err == nil {
		t.Fatalf("expected error for 0644 file")
	}
	if !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("expected ErrMasterKeyMalformed in chain, got: %v", err)
	}
}

func TestNewFileMasterKeyRejectsGroupReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := NewFileMasterKey(path); err == nil || !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("expected ErrMasterKeyMalformed for 0640, got: %v", err)
	}
}

func TestNewFileMasterKeyRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	// 31-byte file (one short).
	if err := os.WriteFile(path, make([]byte, masterKeySize-1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewFileMasterKey(path); err == nil || !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("31-byte file should fail with ErrMasterKeyMalformed, got: %v", err)
	}

	// 33-byte file (one long).
	if err := os.WriteFile(path, make([]byte, masterKeySize+1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewFileMasterKey(path); err == nil || !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("33-byte file should fail with ErrMasterKeyMalformed, got: %v", err)
	}
}

func TestNewFileMasterKeyRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewFileMasterKey(dir); err == nil || !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("directory path should fail with ErrMasterKeyMalformed, got: %v", err)
	}
}

func TestLoadOrMintCreatesIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "master.key") // parent doesn't exist either

	var notified string
	mk, err := LoadOrMintFileMasterKey(path, func(p string) { notified = p })
	if err != nil {
		t.Fatalf("LoadOrMint: %v", err)
	}
	if mk == nil || len(mk.Key()) != masterKeySize {
		t.Fatalf("returned mk = %+v", mk)
	}
	if notified != path {
		t.Errorf("notifyMint called with %q, want %q", notified, path)
	}
	// File should now exist with 0600 perms.
	info, _ := os.Stat(path)
	if info == nil || info.Mode().Perm() != 0o600 {
		t.Errorf("minted file perms: %v", info)
	}
}

func TestLoadOrMintLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Capture the bytes so we can verify they're not re-minted.
	original, _ := os.ReadFile(path)

	var notified string
	mk, err := LoadOrMintFileMasterKey(path, func(p string) { notified = p })
	if err != nil {
		t.Fatalf("LoadOrMint: %v", err)
	}
	// Existing file ⇒ NO notification (this isn't a first-mint event).
	if notified != "" {
		t.Errorf("notifyMint should NOT fire when loading existing key; got %q", notified)
	}
	// And the bytes on disk are unchanged.
	after, _ := os.ReadFile(path)
	if string(original) != string(after) {
		t.Errorf("existing key was modified by LoadOrMint")
	}
	if mk == nil {
		t.Errorf("returned nil mk")
	}
}

func TestLoadOrMintMalformedSurfaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	// 31 bytes ⇒ malformed.
	if err := os.WriteFile(path, make([]byte, masterKeySize-1), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mk, err := LoadOrMintFileMasterKey(path, nil)
	if err == nil {
		t.Fatalf("expected error for malformed file, got mk=%v", mk)
	}
	if !errors.Is(err, ErrMasterKeyMalformed) {
		t.Errorf("expected ErrMasterKeyMalformed (wrapped), got: %v", err)
	}
}

func TestLoadOrMintNilNotifier(t *testing.T) {
	// nil notifier is allowed; auto-mint should still succeed silently.
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	mk, err := LoadOrMintFileMasterKey(path, nil)
	if err != nil {
		t.Fatalf("LoadOrMint(nil notifier): %v", err)
	}
	if mk == nil {
		t.Errorf("returned nil mk")
	}
}

// Round-trip: mint, load, encrypt, decrypt — sanity that the loaded
// key is usable by the crypto primitives.
func TestFileMasterKeyEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := MintFileMasterKey(path); err != nil {
		t.Fatalf("mint: %v", err)
	}
	mk, err := NewFileMasterKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	plaintext := []byte("hello, secret world")
	es, err := Encrypt(mk, plaintext, []byte("aad-outer"), []byte("aad-inner"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(mk, es, []byte("aad-outer"), []byte("aad-inner"))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("round-trip mismatch: %q != %q", got, plaintext)
	}
}
