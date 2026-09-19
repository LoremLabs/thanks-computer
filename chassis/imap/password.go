package imap

import "github.com/loremlabs/thanks-computer/chassis/apppass"

// ErrBadHash is returned when a stored hash is not a PHC argon2id string
// apppass produced. Passwords themselves live in the identity store
// (chassis/authn); an IMAP account holds none.
var ErrBadHash = apppass.ErrBadHash
