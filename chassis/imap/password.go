package imap

import "github.com/loremlabs/thanks-computer/chassis/apppass"

// The password mechanism lives in chassis/apppass, shared with the calendar
// head so one credential can open both. These names are kept so the ops,
// the head and their tests read as they always did.
var (
	// ErrBadHash is returned when a stored hash is not a PHC argon2id string
	// apppass produced.
	ErrBadHash = apppass.ErrBadHash

	HashPassword         = apppass.HashPassword
	VerifyPassword       = apppass.VerifyPassword
	VerifyDummy          = apppass.VerifyDummy
	GeneratePassword     = apppass.GeneratePassword
	GenerateWordPassword = apppass.GenerateWordPassword
)

// WordPasswordDefaultWords is apppass.WordPasswordDefaultWords.
const WordPasswordDefaultWords = apppass.WordPasswordDefaultWords
