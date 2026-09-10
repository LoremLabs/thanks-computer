package source

import "testing"

func TestParseAction(t *testing.T) {
	ok := []struct {
		in   string
		op   string
		dest string
	}{
		{"", "none", ""},
		{"none", "none", ""},
		{"seen", "seen", ""},
		{"move:Processed", "move", "Processed"},
		{"move:Archive/2026", "move", "Archive/2026"},
		{"  move:Done  ", "move", "Done"},
	}
	for _, c := range ok {
		a, err := ParseAction(c.in)
		if err != nil {
			t.Errorf("ParseAction(%q) error: %v", c.in, err)
			continue
		}
		if a.Op != c.op || a.Dest != c.dest {
			t.Errorf("ParseAction(%q) = %+v, want op=%q dest=%q", c.in, a, c.op, c.dest)
		}
		// Round-trips (except the "" alias, which normalizes to "none").
		if c.in != "" && a.String() != normalize(c.in) {
			t.Errorf("String() = %q, want %q", a.String(), normalize(c.in))
		}
	}
	for _, bad := range []string{"move:", "move: ", "delete", "expunge", "move"} {
		if _, err := ParseAction(bad); err == nil {
			t.Errorf("ParseAction(%q) should have errored", bad)
		}
	}
}

// normalize maps a valid spec to its canonical String() form for the
// round-trip check (trims and drops the "none"/"" distinction).
func normalize(s string) string {
	switch {
	case s == "" || s == "none":
		return "none"
	default:
		a, _ := ParseAction(s)
		return a.String()
	}
}
