package authn

import (
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
)

// Every issued password carries the id of its credential:
//
//	people    <short-id>-word-word-word-word-word     k7m2-river-galaxy-bamboo-orbit-velvet
//	machines  txc_<short-id>_<secret>                 txc_k7m2_p3vw8n…
//
// A principal can hold many credentials (one per device, one per printer).
// Without the id a login would have to try every hash on the principal, at
// one 16 MiB argon2 run each. With it a login is one row lookup and one
// verify. The id is not a secret and adds no secrecy: the words or the
// secret carry all of it. It only needs to be unique within its principal,
// because the username has already resolved the principal.
//
// A password the chassis did not issue has no id, so it has no path: it
// fails like any wrong password. That is also why a caller cannot choose a
// password — the chassis must embed the id.

// PasswordStyle picks the issued form.
type PasswordStyle string

const (
	// StyleToken is the machine form, `txc_<short-id>_<secret>`. The default.
	StyleToken PasswordStyle = "token"
	// StyleWords is the form a person types, `<short-id>-word-word-…`.
	StyleWords PasswordStyle = "words"
)

const (
	tokenPrefix = "txc_"
	// tokenSecretLen characters of apppass.Alphabet (31 symbols) ≈ 158 bits.
	tokenSecretLen = 32

	// shortIDLen characters of shortIDAlphabet: 27^4 ≈ 531k ids per
	// principal. A collision is caught by the UNIQUE index and re-rolled.
	shortIDLen = 4
	// shortIDAlphabet is apppass.Alphabet minus the vowels and `y`: the id is
	// the first thing a person reads in their password, and four random
	// letters will eventually spell something. Without vowels they cannot.
	shortIDAlphabet = "bcdfghjkmnpqrstvwxz23456789"

	// Word counts accepted for StyleWords; the default is apppass's.
	MinPasswordWords = 4
	MaxPasswordWords = 12
)

// newShortID is a var so a test can force a collision.
var newShortID = func() string { return apppass.GenerateFrom(shortIDAlphabet, shortIDLen) }

// issuePassword builds the password for a credential with this short id.
func issuePassword(style PasswordStyle, words int, shortID string) (string, error) {
	switch style {
	case StyleToken, "":
		return tokenPrefix + shortID + "_" + apppass.GenerateFrom(apppass.Alphabet, tokenSecretLen), nil
	case StyleWords:
		if words == 0 {
			words = apppass.WordPasswordDefaultWords
		}
		if words < MinPasswordWords || words > MaxPasswordWords {
			return "", fmt.Errorf("password_words must be between %d and %d", MinPasswordWords, MaxPasswordWords)
		}
		return shortID + "-" + apppass.GenerateWordPassword(words), nil
	}
	return "", fmt.Errorf("password_style must be %q or %q", StyleToken, StyleWords)
}

// ShortIDOf extracts the credential short id from a presented password. ok
// is false for anything the chassis could not have issued (a legacy
// password, a typo in the first group) — the caller then burns a dummy
// verify and fails the login like any wrong password.
//
// It accepts any id length from shortIDLen up, so the id can grow later
// without stranding the passwords already issued.
func ShortIDOf(password string) (shortID string, ok bool) {
	var rest string
	if after, isToken := strings.CutPrefix(password, tokenPrefix); isToken {
		shortID, rest, ok = strings.Cut(after, "_")
	} else {
		shortID, rest, ok = strings.Cut(password, "-")
	}
	if !ok || rest == "" || len(shortID) < shortIDLen || len(shortID) > 16 {
		return "", false
	}
	for i := 0; i < len(shortID); i++ {
		if !strings.Contains(shortIDAlphabet, shortID[i:i+1]) {
			return "", false
		}
	}
	return shortID, true
}
