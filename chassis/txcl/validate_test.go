package txcl_test

import (
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// Strict validation flags authoring mistakes the lenient runtime parser
// tolerates. Each case asserts both that Validate reports an error AND
// that Resonator stays lenient (no error) for the same input — the
// guarantee that already-deployed rules don't start failing at runtime.
func TestValidateFlagsLenientlyAcceptedMistakes(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantMatch string // substring expected in the Validate error
	}{
		{
			name:      "unterminated string",
			input:     "SELECT @web.req.url.query.q.0 AS .q\n    DEFAULT \"What is jsx used for?' fmo\n",
			wantMatch: "unterminated string",
		},
		{
			name:      "unknown verb",
			input:     "SELEKT .x AS .y",
			wantMatch: "unexpected token",
		},
		{
			name:      "trailing garbage after clause",
			input:     `SELECT .x AS .y DEFAULT "ok" fmo`,
			wantMatch: "unexpected token",
		},
		{
			name:      "unterminated regex",
			input:     "WHEN .x =~ /^abc",
			wantMatch: "unterminated regex",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := txcl.Validate(tc.input)
			if len(diags) == 0 {
				t.Fatalf("Validate(%q) = no diagnostics, want one containing %q", tc.input, tc.wantMatch)
			}
			joined := strings.Join(diags, " | ")
			if !strings.Contains(joined, tc.wantMatch) {
				t.Fatalf("Validate diagnostics = %q, want substring %q", joined, tc.wantMatch)
			}

			// Runtime parser must stay lenient: no error for the same
			// input (guards against a runtime regression).
			if _, rerr := txcl.Resonator(tc.input); rerr != nil {
				t.Fatalf("Resonator(%q) regressed to error %q — runtime must stay lenient", tc.input, rerr)
			}
		})
	}
}

// A run of garbage after a typo'd verb reports a single diagnostic, not
// one per trailing token — keeps the UI's error count meaningful.
func TestValidateSuppressesCascade(t *testing.T) {
	diags := txcl.Validate("SELEKT .x AS .y")
	if len(diags) != 1 {
		t.Fatalf("Validate cascade = %d diagnostics %q, want exactly 1", len(diags), diags)
	}
}

// Well-formed rules pass strict validation cleanly — strict mode must
// not introduce false positives on valid grammar.
func TestValidateAcceptsWellFormed(t *testing.T) {
	cases := []string{
		`WHEN * SELECT @x AS .y`,
		"# a comment\nSELECT @web.req.url.query.q.0 AS .q DEFAULT \"fallback\"",
		`WHEN ._txc.src == "cron" SELECT @a AS .x, @b AS .y DEFAULT 42 PRIORITY 2`,
		`SET .a = 5, .b = ["x", "y"]`,
		`WHEN .a =~ /^testing/ EXEC "hello-world"`,
		`EMIT .a = 1, .b = "x", .c = [true, 2]`,
		"SELECT @web.req.url.query.question.0\n    AS .question\n    DEFAULT \"What is react used for?\"\n\nEXEC \"mcp+https://x\" WITH timeout = \"60s\", debug = true",
		"", // empty draft is valid
	}
	for _, in := range cases {
		if diags := txcl.Validate(in); len(diags) != 0 {
			t.Errorf("Validate(%q) = %q, want no diagnostics", in, diags)
		}
	}
}
