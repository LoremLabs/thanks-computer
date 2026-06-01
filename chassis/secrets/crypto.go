// Package secrets implements the per-tenant secret store.
//
// See internal docs/todo-secret-store.md (design of record) and
// internal docs/todo-secret-store-implementation.md (implementation arc).
//
// This file holds the cryptographic primitives only. AES-256-GCM
// envelope encryption: each secret is encrypted with a freshly-
// generated per-secret DEK; the DEK is wrapped (AES-256-GCM) by a
// host-local master key. Both layers are AAD-bound to row identity
// so a stolen blob cannot be moved between tenants, secrets, or
// versions without GCM verification failing.
//
// The primitives are row-agnostic — callers supply the AAD bytes.
// This separation lets the Store (PR 2) assemble AAD from row
// fields without the crypto layer knowing the row schema.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const (
	masterKeySize = 32 // AES-256
	dekSize       = 32 // AES-256
	nonceSize     = 12 // AES-GCM standard nonce size
)

// MasterKeyProvider yields the host-local master key.
//
// This interface is the explicit overlay seam. The chassis ships
// FileMasterKey only; a downstream overlay (or any other deployment
// that needs KMS/HSM-backed keys) implements MasterKeyProvider in a
// separate package and registers it without the chassis caring.
// No KMS-vendor code lives in the chassis tree by design — see
// internal docs/todo-secret-store.md §1.6.
type MasterKeyProvider interface {
	// Key returns the 32-byte master key. Callers must not mutate
	// or retain the returned slice beyond the immediate call.
	Key() []byte
	// Version returns the master-key generation. v1 is always 1;
	// multi-version overlap (online MK rotation) is Phase 2.
	Version() int
}

// EncryptedSecret is the on-disk representation of one secret
// version. PR 2's Store places these fields directly into
// tenant_secret_versions row columns.
type EncryptedSecret struct {
	Nonce      []byte // 12-byte AES-GCM nonce (outer / secret layer)
	Ciphertext []byte // ciphertext ‖ GCM tag
	WrappedDEK []byte // DEK encrypted with MK (includes GCM tag)
	DEKNonce   []byte // 12-byte AES-GCM nonce (inner / wrap layer)
	KeyVersion int    // master-key generation that wrapped the DEK
}

// Encrypt seals plaintext with a freshly-generated DEK, then wraps
// the DEK with mk's key. AAD is bound to BOTH layers.
//
// The caller assembles AAD from row identity (see
// internal docs/todo-secret-store.md §3 for the recommended layout):
//
//	outerAAD = tenant_id ‖ secret_id ‖ version_no ‖ name ‖ key_version
//	innerAAD = tenant_id ‖ secret_id ‖ version_no ‖ key_version
//
// Either AAD may be nil; nil and empty are treated identically by
// AES-GCM. The chosen layout is opaque to this package — the
// invariant is only that Encrypt and Decrypt receive the same AAD
// for the same EncryptedSecret.
func Encrypt(mk MasterKeyProvider, plaintext, outerAAD, innerAAD []byte) (*EncryptedSecret, error) {
	if mk == nil {
		return nil, errors.New("secrets: nil MasterKeyProvider")
	}
	key := mk.Key()
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("secrets: master key must be %d bytes, got %d", masterKeySize, len(key))
	}

	// 1. Mint a fresh per-secret DEK.
	dek := make([]byte, dekSize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, fmt.Errorf("secrets: mint DEK: %w", err)
	}
	defer Zero(dek) // wipe the local copy after wrapping

	// 2. Seal plaintext with the DEK (outer layer).
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: mint outer nonce: %w", err)
	}
	dekCipher, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: DEK cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekCipher)
	if err != nil {
		return nil, fmt.Errorf("secrets: DEK GCM: %w", err)
	}
	ciphertext := dekGCM.Seal(nil, nonce, plaintext, outerAAD)

	// 3. Wrap the DEK with the MK (inner layer).
	dekNonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, dekNonce); err != nil {
		return nil, fmt.Errorf("secrets: mint inner nonce: %w", err)
	}
	mkCipher, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: MK cipher: %w", err)
	}
	mkGCM, err := cipher.NewGCM(mkCipher)
	if err != nil {
		return nil, fmt.Errorf("secrets: MK GCM: %w", err)
	}
	wrappedDEK := mkGCM.Seal(nil, dekNonce, dek, innerAAD)

	return &EncryptedSecret{
		Nonce:      nonce,
		Ciphertext: ciphertext,
		WrappedDEK: wrappedDEK,
		DEKNonce:   dekNonce,
		KeyVersion: mk.Version(),
	}, nil
}

// Decrypt verifies AAD on both layers and returns plaintext.
//
// AAD mismatch, ciphertext tampering, or wrap-DEK tampering all
// surface as the same authentication failure — AES-GCM's Open
// returns a generic error and we wrap it. Callers should not
// distinguish among these failure modes; they're all "this blob
// is not what you think it is."
//
// Decrypt fails if es.KeyVersion != mk.Version(). Multi-version MK
// support (where a decrypt path falls back to an older MK
// generation during a rewrap window) is Phase 2.
func Decrypt(mk MasterKeyProvider, es *EncryptedSecret, outerAAD, innerAAD []byte) ([]byte, error) {
	if mk == nil {
		return nil, errors.New("secrets: nil MasterKeyProvider")
	}
	if es == nil {
		return nil, errors.New("secrets: nil EncryptedSecret")
	}
	if es.KeyVersion != mk.Version() {
		return nil, fmt.Errorf("secrets: key version mismatch (stored=%d, provider=%d)", es.KeyVersion, mk.Version())
	}
	key := mk.Key()
	if len(key) != masterKeySize {
		return nil, fmt.Errorf("secrets: master key must be %d bytes, got %d", masterKeySize, len(key))
	}

	// 1. Unwrap DEK with MK.
	mkCipher, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: MK cipher: %w", err)
	}
	mkGCM, err := cipher.NewGCM(mkCipher)
	if err != nil {
		return nil, fmt.Errorf("secrets: MK GCM: %w", err)
	}
	dek, err := mkGCM.Open(nil, es.DEKNonce, es.WrappedDEK, innerAAD)
	if err != nil {
		return nil, fmt.Errorf("secrets: unwrap DEK: %w", err)
	}
	defer Zero(dek)

	// 2. Decrypt ciphertext with DEK.
	dekCipher, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: DEK cipher: %w", err)
	}
	dekGCM, err := cipher.NewGCM(dekCipher)
	if err != nil {
		return nil, fmt.Errorf("secrets: DEK GCM: %w", err)
	}
	plaintext, err := dekGCM.Open(nil, es.Nonce, es.Ciphertext, outerAAD)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}

// Zero overwrites the bytes of b with zeros. Safe to call on nil.
//
// Callers that materialize cleartext should zero their copies when
// done — particularly after using a cleartext to set an outbound
// header, sign a request, or write a derived non-secret value back
// into the envelope. PR 2's SecretBag.Zero() uses this.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
