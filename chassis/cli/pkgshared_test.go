package cli

import "testing"

func TestSplitBaseChannel(t *testing.T) {
	cases := []struct {
		stack, base, channel string
	}{
		{"support", "support", ""},
		{"_mail", "", "_mail"},
		{"support/_mail", "support", "_mail"},
		{"a/b/_cron", "a/b", "_cron"},
		{"hello", "hello", ""},
		{"team/frontend", "team/frontend", ""}, // mid `_`-free → no channel
	}
	for _, c := range cases {
		base, channel := splitBaseChannel(c.stack)
		if base != c.base || channel != c.channel {
			t.Errorf("splitBaseChannel(%q) = (%q,%q), want (%q,%q)",
				c.stack, base, channel, c.base, c.channel)
		}
	}
}
