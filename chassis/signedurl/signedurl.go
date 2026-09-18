// Package signedurl mints and checks the capability tokens behind
// `/_txc/signed/<token>`: a short-lived, self-contained grant to read ONE
// exact document, handed to something outside the chassis (a parser, a
// transcoder) so a large body never has to ride an envelope.
//
// A token is a claim set and an HMAC over it:
//
//	v1.<base64url(JSON claims)>.<base64url(HMAC-SHA256)>
//
// Nothing is stored. Any node holding the same key verifies any node's
// token, which is what lets one web node sign and another serve. The claims
// pin the document's CONTENT (its sha256), not just its address, so a token
// can never be made to read bytes that were not there when it was signed.
//
// A token is a bearer credential for its lifetime. Keep it out of logs.
package signedurl

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
// Part of the wire contract: changing it invalidates every live token.
const KeyLabel = "txco/signed-url/v1"

// PathPrefix is where the web head serves a token: PathPrefix + token.
const PathPrefix = "/_txc/signed/"

const (
	version = "v1"
	// maxTokenLen bounds what Verify will even look at. A real token is
	// ~350 bytes; this is headroom, not a target.
	maxTokenLen = 1024
)

var (
	// ErrInvalid is every way a token can be wrong that is not its age:
	// malformed, forged, signed by another key generation.
	ErrInvalid = errors.New("signedurl: invalid token")
	// ErrExpired is a genuine token past its expiry.
	ErrExpired = errors.New("signedurl: token expired")
)

// Claims is what a token grants: a read of resource Resource in collection
// Collection of tenant Tenant, while its content is still SHA256, until
// Expires.
type Claims struct {
	Tenant     string `json:"t"`
	Collection string `json:"c"` // collection id
	Resource   string `json:"r"` // resource id
	SHA256     string `json:"h"` // bare hex of the pinned content
	Expires    int64  `json:"e"` // unix seconds
	KeyVersion int    `json:"k"` // master-key generation that signed it
}

// Signer signs and verifies tokens under one key.
type Signer struct {
	key        []byte
	keyVersion int
}

// New returns a Signer over key (32 bytes from secrets.DeriveKey) of the
// given master-key generation. The Signer keeps its own copy.
func New(key []byte, keyVersion int) (*Signer, error) {
	if len(key) < 32 {
		return nil, errors.New("signedurl: key must be at least 32 bytes")
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
	if c.Tenant == "" || c.Collection == "" || c.Resource == "" || c.SHA256 == "" || c.Expires <= 0 {
		return "", errors.New("signedurl: incomplete claims")
	}
	c.KeyVersion = s.keyVersion
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := version + "." + base64.RawURLEncoding.EncodeToString(raw)
	return signed + "." + base64.RawURLEncoding.EncodeToString(s.mac(signed)), nil
}

// Verify returns the claims of a genuine, unexpired token. The MAC is
// checked before a single byte of the claims is parsed.
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
	if c.KeyVersion != s.keyVersion || c.Tenant == "" || c.Collection == "" || c.Resource == "" || c.SHA256 == "" {
		return Claims{}, ErrInvalid
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrExpired
	}
	return c, nil
}
