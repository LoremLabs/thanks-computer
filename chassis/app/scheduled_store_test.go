package app

import "testing"

func TestOpenScheduledStore(t *testing.T) {
	cases := []struct {
		name, personalities, store string
		open, polls                bool
	}{
		{"the poller opens the bundled store", "cron,web,scheduled", "sqlite", true, true},
		{"the poller opens a shared store", "cron,web,scheduled", "postgres", true, true},
		{"an admin-only node enqueues into a shared store", "admin", "postgres", true, false},
		{"an admin-only node leaves the node-local store closed", "admin", "sqlite", false, false},
		{"no store named, no poller", "admin", "", false, false},
	}
	for _, c := range cases {
		open, polls := openScheduledStore(c.personalities, c.store)
		if open != c.open || polls != c.polls {
			t.Errorf("%s: open=%v polls=%v, want open=%v polls=%v", c.name, open, polls, c.open, c.polls)
		}
	}
}
