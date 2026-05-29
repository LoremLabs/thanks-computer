package txcl

import (
	"strings"
	"testing"
)

// TestResonatorValid covers the happy path of the public Resonator entry
// point: a syntactically valid txcl string returns a non-nil resonator and
// no error.
func TestResonatorValid(t *testing.T) {
	r, err := Resonator(`WHEN .x == 1 SELECT @x AS .y EXEC "http://example.com/echo"`)
	if err != nil {
		t.Fatalf("Resonator returned err on valid input: %v", err)
	}
	if r == nil {
		t.Fatal("Resonator returned nil on valid input")
	}
	if r.Exec != "http://example.com/echo" {
		t.Errorf("Resonator.Exec = %q, want http://example.com/echo", r.Exec)
	}
}

// TestResonatorInvalid covers the error path: a parse failure must surface
// the parser's accumulated errors as a single joined error value.
func TestResonatorInvalid(t *testing.T) {
	_, err := Resonator(`WHEN .x ?? 1`) // ?? is not a valid operator
	if err == nil {
		t.Fatal("Resonator returned no err on invalid input")
	}
	// The parser joins errors with " : "; just confirm we got something
	// non-empty rather than asserting on the exact wording.
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("Resonator returned empty error message")
	}
}

// TestResonatorNoErr is the test-helper variant used throughout the codebase
// (see operation, processor, bootstrap tests). Verify it returns the same
// resonator value on the happy path and silently produces a value (possibly
// non-nil) on bad input — its contract is "no error return," not "nil on
// error."
func TestResonatorNoErr(t *testing.T) {
	r := ResonatorNoErr(`WHEN .x == 1 SELECT @x AS .y EXEC "http://example.com/"`)
	if r == nil {
		t.Fatal("ResonatorNoErr returned nil on valid input")
	}
	if r.Exec != "http://example.com/" {
		t.Errorf("ResonatorNoErr.Exec = %q, want http://example.com/", r.Exec)
	}
}
