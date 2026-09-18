package apppass

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// BasicHeaderMatches checks an `Authorization: Basic <token>` header value
// against one STATIC credential (a literal user and a password held as a
// secret) — the verdict-only `basic-auth-verify` op and the ipp head's v1
// credential both use it. Not for account tables: those hash with argon2id
// (VerifyPassword).
//
// RFC 7617: the scheme is case-insensitive, the token is
// base64(user-id ":" password), and a user-id may not contain a colon. Both
// halves are compared in constant time and BOTH comparisons always run, so
// timing does not reveal which half failed; length differences are the one
// thing ConstantTimeCompare reveals, the same trade-off hmac.Equal makes.
// The decoded credential is zeroed before returning.
func BasicHeaderMatches(header, user string, password []byte) bool {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Basic") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	defer func() {
		for i := range raw {
			raw[i] = 0
		}
	}()
	colon := bytes.IndexByte(raw, ':')
	if colon < 0 {
		return false
	}
	u := subtle.ConstantTimeCompare(raw[:colon], []byte(user))
	p := subtle.ConstantTimeCompare(raw[colon+1:], password)
	return u&p == 1
}
