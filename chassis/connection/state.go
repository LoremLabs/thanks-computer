package connection

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The OAuth `state` is a claim set and an HMAC over it, keyed from the
// secret store's master key, so any node of a fleet checks any node's
// state with nothing to look up:
//
//	v1.<base64url(JSON claims)>.<base64url(HMAC-SHA256)>
//
// It is not what makes a flow single-use: a flow row (store.go) is, consumed
// before the code is exchanged. The signature is what lets `finish` refuse
// a forged or foreign state before it touches the store.
//
// The PKCE verifier is derived from the nonce under the same key, so an
// authorization code seen in a callback's trace is useless without the
// chassis: the provider will not exchange it without the verifier.

// KeyLabel is the derivation label of the signing key (secrets.DeriveKey).
// Part of the wire contract: changing it invalidates every live flow.
const KeyLabel = "txco/connection/v1"

const (
	stateVersion = "v1"
	maxStateLen  = 1024
	// FlowTTL is how long a sign-in may take.
	FlowTTL = 10 * time.Minute
)

var (
	// ErrBadState: malformed, forged, or signed under another key.
	ErrBadState = errors.New("connection: bad state")
	// ErrStateExpired: a genuine state past its expiry.
	ErrStateExpired = errors.New("connection: state expired")
)

// stateClaims is what a state carries. The nonce is the flow's key.
type stateClaims struct {
	Tenant     string `json:"t"`
	Stack      string `json:"s"`
	Provider   string `json:"p"`
	Nonce      string `json:"n"` // base64url, 32 random bytes
	Expires    int64  `json:"e"` // unix seconds
	KeyVersion int    `json:"k"`
}

// Signer signs states and derives verifiers under one key.
type Signer struct {
	key        []byte
	keyVersion int
}

// NewSigner returns a Signer over key (32 bytes from secrets.DeriveKey) of
// the given master-key generation. The Signer keeps its own copy.
func NewSigner(key []byte, keyVersion int) (*Signer, error) {
	if len(key) < 32 {
		return nil, errors.New("connection: key must be at least 32 bytes")
	}
	return &Signer{key: append([]byte(nil), key...), keyVersion: keyVersion}, nil
}

func (s *Signer) mac(purpose, data string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose))
	m.Write([]byte{0})
	m.Write([]byte(data))
	return m.Sum(nil)
}

// newNonce is 32 random bytes, base64url.
func newNonce() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// nonceHash is the flow row's key: the nonce never sits in the store as it
// sits in the browser's URL.
func nonceHash(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(sum[:])
}

// sign returns the state for c, stamped with the key generation.
func (s *Signer) sign(c stateClaims) (string, error) {
	if c.Tenant == "" || c.Stack == "" || c.Provider == "" || c.Nonce == "" || c.Expires <= 0 {
		return "", errors.New("connection: incomplete state")
	}
	c.KeyVersion = s.keyVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := stateVersion + "." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(s.mac("state", signed)), nil
}

// verify returns the claims of a genuine, unexpired state. The MAC is
// checked before a byte of the claims is parsed.
func (s *Signer) verify(state string, now time.Time) (stateClaims, error) {
	if len(state) == 0 || len(state) > maxStateLen {
		return stateClaims{}, ErrBadState
	}
	parts := strings.Split(state, ".")
	if len(parts) != 3 || parts[0] != stateVersion {
		return stateClaims{}, ErrBadState
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return stateClaims{}, ErrBadState
	}
	if !hmac.Equal(got, s.mac("state", parts[0]+"."+parts[1])) {
		return stateClaims{}, ErrBadState
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return stateClaims{}, ErrBadState
	}
	var c stateClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return stateClaims{}, ErrBadState
	}
	if c.KeyVersion != s.keyVersion || c.Tenant == "" || c.Stack == "" || c.Provider == "" || c.Nonce == "" {
		return stateClaims{}, ErrBadState
	}
	if now.Unix() >= c.Expires {
		return stateClaims{}, ErrStateExpired
	}
	return c, nil
}

// verifier is the PKCE code verifier for a flow: 43 characters of the
// unreserved alphabet, derived from the nonce so no node stores it.
func (s *Signer) verifier(nonce string) string {
	return base64.RawURLEncoding.EncodeToString(s.mac("pkce", nonce))
}

// challenge is the S256 code challenge of a verifier.
func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// browserToken is the per-flow cookie value: random, hashed in the row.
func browserToken() (value, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	value = base64.RawURLEncoding.EncodeToString(b[:])
	return value, nonceHash(value), nil
}
