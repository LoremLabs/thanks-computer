package secrets

import (
	"errors"
	"strings"
)

// FormatPlaceholder is the substitution token in a `format` template.
// Exactly one occurrence required; everything else is treated as
// literal characters. The placeholder is deliberately not printf-
// style (`%s`) to (a) close the printf-injection surface and (b)
// make "single substitution point" a structural property rather than
// a discipline.
const FormatPlaceholder = "{}"

// ErrInvalidFormat is returned when a format template is missing the
// placeholder, has more than one occurrence, or is otherwise mal-
// formed. Admin handlers / op handlers translate to a 400 with code
// `invalid_format`.
var ErrInvalidFormat = errors.New("secrets: format template must contain exactly one '{}' placeholder")

// Substitute applies a literal-with-one-slot template. The template
// must contain exactly one occurrence of `{}`; cleartext fills that
// slot. Anything else in the template (including bytes that happen
// to look like `{` or `}` individually) is treated as a literal.
//
// Examples:
//
//	Substitute("Bearer {}", []byte("sk_live_abc")) → "Bearer sk_live_abc"
//	Substitute("{}", []byte("sk_live_abc"))        → "sk_live_abc"
//	Substitute("token {} expires", []byte("xyz"))  → "token xyz expires"
//
// Errors:
//
//	Substitute("no placeholder",   []byte("v")) → ErrInvalidFormat
//	Substitute("two {} and {}",    []byte("v")) → ErrInvalidFormat
//
// An empty cleartext is allowed and substitutes as the empty string —
// the format template's surrounding literal still applies. This is
// useful for a "rotate to empty then re-issue" workflow, but
// op handlers should generally reject empty cleartext one layer up.
func Substitute(format string, cleartext []byte) (string, error) {
	count := strings.Count(format, FormatPlaceholder)
	switch count {
	case 0:
		return "", ErrInvalidFormat
	case 1:
		return strings.Replace(format, FormatPlaceholder, string(cleartext), 1), nil
	default:
		return "", ErrInvalidFormat
	}
}

// ValidateFormat reports whether a format template is well-formed
// without actually substituting anything. Useful in two places:
//
//   - Admin handlers validating an inbound `secrets.<path>.format`
//     declaration before persisting the txcl rule.
//   - The processor splice doing a fast-fail pre-check before
//     materializing any cleartext (so a typo in format doesn't
//     decrypt and then throw the cleartext on the floor).
func ValidateFormat(format string) error {
	if strings.Count(format, FormatPlaceholder) != 1 {
		return ErrInvalidFormat
	}
	return nil
}
