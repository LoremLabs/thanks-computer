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
		{
			// An unregistered function in WHEN evaluates to no-match at
			// runtime (silently), so strict mode flags the typo where a
			// human can see it. Lenient parse must still accept it — a
			// rule authored for a newer chassis must load on an older.
			name:      "unknown function in WHEN",
			input:     `WHEN .x == &nope(1)`,
			wantMatch: "unknown function &nope",
		},
		{
			name:      "unknown function after in",
			input:     `WHEN .x in &nope(1)`,
			wantMatch: "unknown function &nope",
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

// A `$[path.json]` that reaches the parser was never expanded by txco.
// Unlike the lenient cases above it is an error on both paths: the
// directive has no runtime meaning, and a bare `$` was a parse error
// before the token existed.
func TestUnexpandedFileLiteralIsAnError(t *testing.T) {
	for _, in := range []string{
		`WHEN .x in $[nouns.json] EXEC "hello-world"`,
		`WHEN .x == $[one.json] EXEC "hello-world"`,
		`SET .o = $[schemas/reply.json]`,
		`EXEC "ai://chat" WITH schema = $[schemas/reply.json]`,
		`EMIT .rows = [1, $[rows.json]]`,
	} {
		diags := txcl.Validate(in)
		if len(diags) == 0 || !strings.Contains(strings.Join(diags, " | "), "expanded by txco before upload") {
			t.Errorf("Validate(%q) = %q, want the unexpanded-literal diagnostic", in, diags)
		}
		if _, err := txcl.Resonator(in); err == nil {
			t.Errorf("Resonator(%q) accepted an unexpanded $[...] literal", in)
		}
	}
	// Mentioned inside a string or a comment it is text, not a directive.
	for _, in := range []string{
		`SET .s = "see $[schemas/reply.json]"`,
		"# $[comment.json]\nSET .s = 1",
	} {
		if diags := txcl.Validate(in); len(diags) != 0 {
			t.Errorf("Validate(%q) = %q, want no diagnostics", in, diags)
		}
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
		`SELECT *`,
		`WHEN * SELECT * EXEC "hello-world"`,
		`SELECT .a, .b AS .c DEFAULT 1 EXEC "hello-world"`,
		`SELECT .a DEFAULT "x"`,
		"# a comment\nSELECT @web.req.url.query.q.0 AS .q DEFAULT \"fallback\"",
		`WHEN ._txc.src == "cron" SELECT @a AS .x, @b AS .y DEFAULT 42 PRIORITY 2`,
		`SET .a = 5, .b = ["x", "y"]`,
		`WHEN .a =~ /^testing/ EXEC "hello-world"`,
		`EMIT .a = 1, .b = "x", .c = [true, 2]`,
		`WHEN .h == &add(1, &mul(2, 3)) EXEC "hello-world"`,
		// The documented &tz usage from docs/advanced/protocols/cron.md —
		// shipped broken pre-Stage-2, pinned working here.
		"WHEN @cron.hour == &tz(\"Asia/Kolkata\", \"hour\", 9)\n  && @cron.minute == &tz(\"Asia/Kolkata\", \"minute\", 9)\nEXEC \"https://ops.example.com/morning-sweep\"",
		"SELECT @web.req.url.query.question.0\n    AS .question\n    DEFAULT \"What is react used for?\"\n\nEXEC \"mcp+https://x\" WITH timeout = \"60s\", debug = true",
		// Membership, presence, and JSON-complete literals.
		`WHEN exists .a && .b in ["x", "y"] EXEC "hello-world"`,
		`WHEN !(.s in ["a", "b"]) || !exists @call.input.run EXEC "hello-world"`,
		`WHEN .code in &array(200, 201) EXEC "hello-world"`,
		`SET .o = {"enabled": true, "limits": {"max": 10, "tags": ["a", null]}, "optional": null}`,
		`EXEC "https://api.example.com/jobs/42" LOOP UNTIL .status in ["done", "failed"] MAX 3`,
		"", // empty draft is valid
	}
	for _, in := range cases {
		if diags := txcl.Validate(in); len(diags) != 0 {
			t.Errorf("Validate(%q) = %q, want no diagnostics", in, diags)
		}
	}
}
