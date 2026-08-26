package opname

import "testing"

// '_' is legal in every stack-name segment (the `_mail`/`_cron` channels, and
// tenant names like `my_book`) AND is a SQL LIKE single-character wildcard.
// EscapeLike is what keeps a LIKE match literal; seg's doc comment points here.
func TestEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"_mail", `\_mail`},
		{"pony/_mail", `pony/\_mail`},
		{"my_book", `my\_book`},
		{"plain", "plain"},
		{"", ""},
		// '%' is banned in a stack name, so in a caller-supplied prefix it can
		// only be an injection attempt — escape it too.
		{"a%b", `a\%b`},
		// The escape char itself is doubled FIRST, so it can't be smuggled in
		// to neutralise the '_' escape that follows.
		{`a\_b`, `a\\\_b`},
	}
	for _, tc := range cases {
		if got := EscapeLike(tc.in); got != tc.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The channel convention depends on '_' staying legal. If someone "fixes" the
// LIKE hazard by banning it here instead, every `_mail`/`_cron`/`_sys` stack
// becomes unnameable — this test is the tripwire.
func TestUnderscoreStaysLegalInNames(t *testing.T) {
	for _, name := range []string{"_mail", "_cron", "_sys/boot", "pony/_mail", "my_book"} {
		if err := ValidStack(name); err != nil {
			t.Errorf("ValidStack(%q) = %v, want nil — the channel convention needs '_'", name, err)
		}
	}
}
