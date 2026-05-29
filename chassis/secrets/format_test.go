package secrets

import (
	"errors"
	"strings"
	"testing"
)

func TestSubstituteHappy(t *testing.T) {
	cases := []struct {
		name   string
		format string
		ct     string
		want   string
	}{
		{"bare placeholder", "{}", "sk_live_abc", "sk_live_abc"},
		{"bearer prefix", "Bearer {}", "abc123", "Bearer abc123"},
		{"github legacy", "token {}", "ghp_abc", "token ghp_abc"},
		{"surrounded", "<<{}>>", "v", "<<v>>"},
		{"with literal braces alone", "{ {} }", "v", "{ v }"},
		{"empty cleartext", "Bearer {}", "", "Bearer "},
		{"placeholder at end", "secret={}", "v", "secret=v"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Substitute(c.format, []byte(c.ct))
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("Substitute(%q, %q) = %q, want %q", c.format, c.ct, got, c.want)
			}
		})
	}
}

func TestSubstituteRejectsMissingPlaceholder(t *testing.T) {
	cases := []string{
		"no placeholder",
		"",
		"only {",
		"only }",
		"%s",       // not a printf format
		"{name}",   // not the bare {}
		"{ }",      // space inside braces ≠ {}
	}
	for _, format := range cases {
		t.Run(format, func(t *testing.T) {
			_, err := Substitute(format, []byte("v"))
			if !errors.Is(err, ErrInvalidFormat) {
				t.Errorf("format %q: expected ErrInvalidFormat, got: %v", format, err)
			}
		})
	}
}

func TestSubstituteRejectsMultiplePlaceholders(t *testing.T) {
	_, err := Substitute("first {} and second {}", []byte("v"))
	if !errors.Is(err, ErrInvalidFormat) {
		t.Errorf("two placeholders: expected ErrInvalidFormat, got: %v", err)
	}
	_, err = Substitute("{}{}", []byte("v"))
	if !errors.Is(err, ErrInvalidFormat) {
		t.Errorf("adjacent placeholders: expected ErrInvalidFormat, got: %v", err)
	}
}

func TestSubstituteCleartextDoesNotReplaceItsOwnPlaceholder(t *testing.T) {
	// If cleartext itself contains `{}`, that's just literal data;
	// it must NOT be re-substituted. strings.Replace with count=1
	// gives this for free; we pin it as a contract.
	got, err := Substitute("Bearer {}", []byte("{}.{}"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Bearer {}.{}" {
		t.Errorf("expected literal cleartext containing braces, got %q", got)
	}
}

func TestSubstituteIsPrintfImmune(t *testing.T) {
	// The format token is NOT %s; printf-style directives in either
	// the template or cleartext have no special meaning.
	for _, s := range []string{"%s", "%v", "%n", "%!s(MISSING)"} {
		got, err := Substitute("X-Format: {}", []byte(s))
		if err != nil {
			t.Fatalf("err for %q: %v", s, err)
		}
		want := "X-Format: " + s
		if got != want {
			t.Errorf("Substitute(_, %q) = %q, want %q", s, got, want)
		}
	}
}

func TestValidateFormat(t *testing.T) {
	for _, format := range []string{"{}", "Bearer {}", "<{}>"} {
		if err := ValidateFormat(format); err != nil {
			t.Errorf("format %q should validate, got: %v", format, err)
		}
	}
	for _, format := range []string{"", "no slot", "two {} and {}"} {
		if err := ValidateFormat(format); err == nil {
			t.Errorf("format %q should fail to validate", format)
		}
	}
}

// TestSubstituteEmptyFormatExplicitlyFails pins the contract: an
// empty format string is invalid (no placeholder), not "treat as
// raw substitution". For raw substitution the operator should
// simply omit the .format key, which the processor splice handles
// at the caller level.
func TestSubstituteEmptyFormatExplicitlyFails(t *testing.T) {
	_, err := Substitute("", []byte("v"))
	if !errors.Is(err, ErrInvalidFormat) {
		t.Errorf("empty format: expected ErrInvalidFormat, got: %v", err)
	}
	// And the error message should mention the placeholder so the
	// operator gets a useful clue.
	if err != nil && !strings.Contains(err.Error(), "{}") {
		t.Errorf("error message should mention '{}', got: %v", err)
	}
}
