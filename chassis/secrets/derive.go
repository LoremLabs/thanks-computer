package secrets

import (
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
)

// DeriveKey returns a 32-byte subkey of the master key for ONE purpose,
// named by label ("txco/signed-url/v1"), plus the master-key generation it
// came from.
//
// It exists so a chassis feature that needs a fleet-wide symmetric key (an
// HMAC over a capability URL, say) does not grow a key of its own to
// provision, sync and rotate: every node that can decrypt the fleet's
// secrets already holds the same master key, so every node derives the same
// subkey. HKDF's info parameter is the domain separation — a subkey is
// useless for any other label, and nothing about the master key can be
// recovered from one.
//
// The label is part of the contract: changing it invalidates everything
// signed under the old one. The caller owns the returned slice.
func DeriveKey(mk MasterKeyProvider, label string) ([]byte, int, error) {
	if mk == nil {
		return nil, 0, errors.New("secrets: nil MasterKeyProvider")
	}
	if label == "" {
		return nil, 0, errors.New("secrets: empty derivation label")
	}
	k := mk.Key()
	if len(k) != masterKeySize {
		return nil, 0, errors.New("secrets: master key unavailable")
	}
	out, err := hkdf.Key(sha256.New, k, nil, label, masterKeySize)
	if err != nil {
		return nil, 0, err
	}
	return out, mk.Version(), nil
}
