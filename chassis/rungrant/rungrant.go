// Package rungrant mints and checks the token that travels with one piece of
// work: proof that the chassis dispatched it, and the id of the run grant
// row that says what it may ask for.
//
// A token is a claim set and an HMAC over it, the form of a signed URL
// (chassis/signedurl) under a key of its own:
//
//	rg1.<base64url(JSON claims)>.<base64url(HMAC-SHA256)>
//
// The token IDENTIFIES; the row AUTHORIZES. A token carries no allowlist and
// no budget, so a genuine token is never enough on its own: every request
// reads the row (chassis/authn), where revocation, a newer generation and a
// spent budget live. Nothing is stored here, and any node holding the same
// key verifies any node's token.
//
// A token is a bearer credential until it expires. Keep it out of logs,
// traces and envelopes.
package rungrant

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// KeyLabel is the derivation label of the signing key (secrets.DeriveKey).
// Part of the wire contract: changing it invalidates every live token. It
// differs from every other label, so a token of another kind (a signed URL)
// never verifies here, nor one of these there.
const KeyLabel = "txco/run-grant/v1"

const (
	version = "rg1"
	// Prefix starts every token, for a scrubber that finds one by its shape.
	Prefix = version + "."
	// maxTokenLen bounds what Verify will even look at. A real token is
	// about 350 bytes.
	maxTokenLen = 1024
)

var (
	// ErrInvalid is every way a token can be wrong that is not its age:
	// malformed, forged, signed by another key generation.
	ErrInvalid = errors.New("rungrant: invalid token")
	// ErrExpired is a genuine token past its expiry.
	ErrExpired = errors.New("rungrant: token expired")
)

// Claims is what a token says: run grant Grant of tenant Tenant was minted
// for Principal's run Run at generation Generation, and is good until
// Expires. Every field but Depth and Trace is required.
type Claims struct {
	Grant      string `json:"g"` // run grant id
	Tenant     string `json:"t"` // tenant id, not its slug
	Principal  string `json:"p"`
	Run        string `json:"r"`
	Generation int64  `json:"n"`
	Depth      int    `json:"d,omitempty"`
	Trace      string `json:"x,omitempty"` // the request that minted the grant
	Expires    int64  `json:"e"`           // unix seconds
	KeyVersion int    `json:"k"`           // master-key generation that signed it
}

func (c Claims) complete() bool {
	return c.Grant != "" && c.Tenant != "" && c.Principal != "" && c.Run != "" &&
		c.Generation > 0 && c.Depth >= 0 && c.Expires > 0
}

// Signer signs and verifies tokens under one key.
type Signer struct {
	key        []byte
	keyVersion int
}

// New returns a Signer over key (32 bytes from secrets.DeriveKey with
// KeyLabel) of the given master-key generation. The Signer keeps its own
// copy.
func New(key []byte, keyVersion int) (*Signer, error) {
	if len(key) < 32 {
		return nil, errors.New("rungrant: key must be at least 32 bytes")
	}
	return &Signer{key: append([]byte(nil), key...), keyVersion: keyVersion}, nil
}

func (s *Signer) mac(signed string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(signed))
	return m.Sum(nil)
}

// Sign returns the token for c. The Signer stamps its own key generation.
func (s *Signer) Sign(c Claims) (string, error) {
	if !c.complete() {
		return "", errors.New("rungrant: incomplete claims")
	}
	c.KeyVersion = s.keyVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := Prefix + base64.RawURLEncoding.EncodeToString(raw)
	token := signed + "." + base64.RawURLEncoding.EncodeToString(s.mac(signed))
	if len(token) > maxTokenLen {
		return "", errors.New("rungrant: claims too long for a token")
	}
	return token, nil
}

// Verify returns the claims of a genuine, unexpired token. The MAC is
// checked before a single byte of the claims is parsed.
//
// A nil error says the chassis signed this token and it has not expired. It
// does NOT say the grant still holds: read the row.
func (s *Signer) Verify(token string, now time.Time) (Claims, error) {
	if len(token) == 0 || len(token) > maxTokenLen {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != version {
		return Claims{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	if !hmac.Equal(got, s.mac(parts[0]+"."+parts[1])) {
		return Claims{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, ErrInvalid
	}
	if c.KeyVersion != s.keyVersion || !c.complete() {
		return Claims{}, ErrInvalid
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrExpired
	}
	return c, nil
}
