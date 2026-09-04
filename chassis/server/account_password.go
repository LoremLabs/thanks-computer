package server

import (
	"fmt"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
)

// accountPassword is the outcome of the password block every account op
// shares (txco://imap/account, txco://calendar/account): the hash to store
// (empty ⇒ leave unchanged), the generated password to return exactly once
// (empty ⇒ none), and whether an existing account's password was rotated.
type accountPassword struct {
	hash      string
	generated string
	rotated   bool
}

// resolveAccountPassword reads `password`, `rotate`, `password_style` and
// `password_words` off the WITH clause:
//
//	password absent   ⇒ unchanged on update / generated on create
//	password ""       ⇒ generated (and returned once)
//	password "…"      ⇒ stored (≥ 8 chars)
//	rotate = true     ⇒ generated even for an existing account
//	password_style    token (default; 24-char group token, ~116 bits) or
//	                  words (`password_words` BIP-39 words, default 5)
//
// exists reports whether the account already exists (consulted only when
// needed). code is "" on success, else "invalid_arg" or "store" for the
// caller to prefix with its op family.
func resolveAccountPassword(meta []byte, exists func() (bool, error)) (res accountPassword, code, msg string) {
	style := gjson.GetBytes(meta, "password_style").String()
	if style == "" {
		style = "token"
	}
	words := apppass.WordPasswordDefaultWords
	if w := gjson.GetBytes(meta, "password_words"); w.Exists() {
		words = int(w.Int())
		if words < 4 || words > 12 {
			return res, "invalid_arg", "password_words must be between 4 and 12"
		}
	}
	var generate func() string
	switch style {
	case "token":
		generate = apppass.GeneratePassword
	case "words":
		generate = func() string { return apppass.GenerateWordPassword(words) }
	default:
		return res, "invalid_arg", "password_style must be \"token\" or \"words\""
	}
	rotate := gjson.GetBytes(meta, "rotate").Bool()
	pw := gjson.GetBytes(meta, "password")
	var err error
	switch {
	case pw.Exists() && pw.String() != "":
		if len(pw.String()) < 8 {
			return res, "invalid_arg", "password must be at least 8 characters"
		}
		res.hash, err = apppass.HashPassword(pw.String())
	case pw.Exists() || rotate:
		res.generated = generate()
		res.hash, err = apppass.HashPassword(res.generated)
		if rotate && err == nil {
			ok, gerr := exists()
			if gerr != nil {
				err = gerr
			}
			res.rotated = ok
		}
	default:
		ok, gerr := exists()
		switch {
		case gerr != nil:
			err = gerr
		case !ok:
			res.generated = generate()
			res.hash, err = apppass.HashPassword(res.generated)
		}
	}
	if err != nil {
		return accountPassword{}, "store", fmt.Sprintf("%v", err)
	}
	return res, "", ""
}
