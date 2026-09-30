package server

import (
	"testing"
)

func TestPersonalityOnMatchesWholeNames(t *testing.T) {
	for _, tc := range []struct {
		list, name string
		want       bool
	}{
		{"cron,web,admin", "web", true},
		{"cron, web ,admin", "web", true},
		{"cron,web,admin,grant", "grant", true},
		{"cron,web,admin", "grant", false},
		{"", "grant", false},
		// A name that is part of another is not that name.
		{"cron,websocket", "web", false},
		{"cron,grants", "grant", false},
		{"cron,regrant", "grant", false},
		{"cron,grant-debug", "grant", false},
	} {
		if got := personalityOn(tc.list, tc.name); got != tc.want {
			t.Errorf("personalityOn(%q, %q) = %v, want %v", tc.list, tc.name, got, tc.want)
		}
	}
	// A personality this node does not run does nothing, twice over.
	var c controller = idle{}
	c.Start()
	c.Stop()
	c.Stop()
}
