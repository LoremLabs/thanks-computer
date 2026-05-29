package secrets

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

// mockMK is a test-only MasterKeyProvider with a known key.
type mockMK struct {
	key []byte
	ver int
}

func (m *mockMK) Key() []byte  { return m.key }
func (m *mockMK) Version() int { return m.ver }

func newMockMK(t *testing.T, version int) *mockMK {
	t.Helper()
	key := make([]byte, masterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("mint mock key: %v", err)
	}
	return &mockMK{key: key, ver: version}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	mk := newMockMK(t, 1)
	plaintext := []byte("sk_live_super_secret_stripe_key_abc123")
	outerAAD := []byte("tnt_abc|sec_xyz|1|STRIPE_API_KEY|1")
	innerAAD := []byte("tnt_abc|sec_xyz|1|1")

	es, err := Encrypt(mk, plaintext, outerAAD, innerAAD)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if es.KeyVersion != 1 {
		t.Errorf("KeyVersion = %d, want 1", es.KeyVersion)
	}
	if len(es.Nonce) != nonceSize {
		t.Errorf("Nonce size = %d, want %d", len(es.Nonce), nonceSize)
	}
	if len(es.DEKNonce) != nonceSize {
		t.Errorf("DEKNonce size = %d, want %d", len(es.DEKNonce), nonceSize)
	}
	if bytes.Contains(es.Ciphertext, plaintext) {
		t.Errorf("ciphertext leaks plaintext")
	}
	if bytes.Contains(es.WrappedDEK, plaintext) {
		t.Errorf("wrapped DEK leaks plaintext")
	}

	got, err := Decrypt(mk, es, outerAAD, innerAAD)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt returned %q, want %q", got, plaintext)
	}
}

func TestEncryptProducesFreshNoncesAndDEKs(t *testing.T) {
	mk := newMockMK(t, 1)
	plaintext := []byte("payload")
	aad := []byte("aad")

	es1, err := Encrypt(mk, plaintext, aad, aad)
	if err != nil {
		t.Fatalf("Encrypt 1: %v", err)
	}
	es2, err := Encrypt(mk, plaintext, aad, aad)
	if err != nil {
		t.Fatalf("Encrypt 2: %v", err)
	}

	// Same plaintext, same AAD, but every variable component must
	// be freshly randomized.
	if bytes.Equal(es1.Nonce, es2.Nonce) {
		t.Errorf("outer nonces reused across encrypts")
	}
	if bytes.Equal(es1.DEKNonce, es2.DEKNonce) {
		t.Errorf("inner (DEK) nonces reused across encrypts")
	}
	if bytes.Equal(es1.WrappedDEK, es2.WrappedDEK) {
		t.Errorf("wrapped DEKs reused — DEK is not fresh per encrypt")
	}
	if bytes.Equal(es1.Ciphertext, es2.Ciphertext) {
		t.Errorf("ciphertexts identical — IND-CPA is broken")
	}
}

func TestDecryptTamperedOuterAAD(t *testing.T) {
	mk := newMockMK(t, 1)
	es, err := Encrypt(mk, []byte("payload"), []byte("outer-aad"), []byte("inner-aad"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt(mk, es, []byte("Outer-aad"), []byte("inner-aad")); err == nil {
		t.Errorf("Decrypt with tampered outer AAD should fail")
	}
}

func TestDecryptTamperedInnerAAD(t *testing.T) {
	mk := newMockMK(t, 1)
	es, err := Encrypt(mk, []byte("payload"), []byte("outer-aad"), []byte("inner-aad"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := Decrypt(mk, es, []byte("outer-aad"), []byte("Inner-aad")); err == nil {
		t.Errorf("Decrypt with tampered inner AAD should fail")
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	mk := newMockMK(t, 1)
	es, err := Encrypt(mk, []byte("payload"), nil, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	es.Ciphertext[0] ^= 0xff
	if _, err := Decrypt(mk, es, nil, nil); err == nil {
		t.Errorf("Decrypt with tampered ciphertext should fail")
	}
}

func TestDecryptTamperedWrappedDEK(t *testing.T) {
	mk := newMockMK(t, 1)
	es, err := Encrypt(mk, []byte("payload"), nil, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	es.WrappedDEK[0] ^= 0xff
	if _, err := Decrypt(mk, es, nil, nil); err == nil {
		t.Errorf("Decrypt with tampered WrappedDEK should fail")
	}
}

// TestAntiSwap is the load-bearing crypto test. Two secrets, encrypted
// under the same MK with their own AADs. Attempt to decrypt each
// blob with the OTHER row's AAD — both attempts must fail. This is
// the invariant that prevents an attacker with DB write access from
// moving a ciphertext from one tenant/secret/version to another.
func TestAntiSwap(t *testing.T) {
	mk := newMockMK(t, 1)

	aadA_outer := []byte("tnt_A|sec_A|1|STRIPE|1")
	aadA_inner := []byte("tnt_A|sec_A|1|1")
	aadB_outer := []byte("tnt_B|sec_B|1|SLACK|1")
	aadB_inner := []byte("tnt_B|sec_B|1|1")

	esA, err := Encrypt(mk, []byte("payload-A"), aadA_outer, aadA_inner)
	if err != nil {
		t.Fatalf("Encrypt A: %v", err)
	}
	esB, err := Encrypt(mk, []byte("payload-B"), aadB_outer, aadB_inner)
	if err != nil {
		t.Fatalf("Encrypt B: %v", err)
	}

	if _, err := Decrypt(mk, esA, aadB_outer, aadB_inner); err == nil {
		t.Errorf("decrypt blob A with row B's AAD: should fail (anti-swap)")
	}
	if _, err := Decrypt(mk, esB, aadA_outer, aadA_inner); err == nil {
		t.Errorf("decrypt blob B with row A's AAD: should fail (anti-swap)")
	}

	// And confirm each blob still decrypts under its own AAD (we
	// haven't accidentally broken the happy path).
	gotA, err := Decrypt(mk, esA, aadA_outer, aadA_inner)
	if err != nil {
		t.Fatalf("Decrypt A: %v", err)
	}
	if !bytes.Equal(gotA, []byte("payload-A")) {
		t.Errorf("Decrypt A returned %q, want payload-A", gotA)
	}
	gotB, err := Decrypt(mk, esB, aadB_outer, aadB_inner)
	if err != nil {
		t.Fatalf("Decrypt B: %v", err)
	}
	if !bytes.Equal(gotB, []byte("payload-B")) {
		t.Errorf("Decrypt B returned %q, want payload-B", gotB)
	}
}

func TestDecryptKeyVersionMismatch(t *testing.T) {
	mkV1 := newMockMK(t, 1)
	es, err := Encrypt(mkV1, []byte("payload"), nil, nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Same key bytes, but the provider claims a different version
	// generation. Decrypt must refuse on version mismatch alone.
	mkV2 := &mockMK{key: mkV1.key, ver: 2}
	_, err = Decrypt(mkV2, es, nil, nil)
	if err == nil {
		t.Fatalf("Decrypt with mismatched KeyVersion should fail")
	}
	if !strings.Contains(err.Error(), "key version mismatch") {
		t.Errorf("expected 'key version mismatch' in error, got: %v", err)
	}
}

func TestEncryptNilMK(t *testing.T) {
	if _, err := Encrypt(nil, []byte("payload"), nil, nil); err == nil {
		t.Errorf("Encrypt(nil, …) should error")
	}
}

func TestDecryptNilEncryptedSecret(t *testing.T) {
	mk := newMockMK(t, 1)
	if _, err := Decrypt(mk, nil, nil, nil); err == nil {
		t.Errorf("Decrypt(…, nil) should error")
	}
}

func TestEncryptWrongKeySize(t *testing.T) {
	mk := &mockMK{key: make([]byte, 16), ver: 1} // wrong size (AES-128 key)
	if _, err := Encrypt(mk, []byte("payload"), nil, nil); err == nil {
		t.Errorf("Encrypt with 16-byte key should error")
	}
}

func TestEncryptNilAAD(t *testing.T) {
	// nil AAD is valid — AES-GCM treats nil and empty identically.
	mk := newMockMK(t, 1)
	es, err := Encrypt(mk, []byte("payload"), nil, nil)
	if err != nil {
		t.Fatalf("Encrypt nil AAD: %v", err)
	}
	got, err := Decrypt(mk, es, nil, nil)
	if err != nil {
		t.Fatalf("Decrypt nil AAD: %v", err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Errorf("got %q want payload", got)
	}
}

func TestZero(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5}
	Zero(b)
	for i, v := range b {
		if v != 0 {
			t.Errorf("Zero failed at index %d: got %d", i, v)
		}
	}
	Zero(nil) // must not panic
}
