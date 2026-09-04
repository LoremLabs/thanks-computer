package app

import (
	"reflect"
	"testing"
)

// TestRoomAlias pins the argv[0] dispatch: invoked as `thanks`, the binary
// behaves as `txco room`; invoked as anything else, argv is untouched.
func TestRoomAlias(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"thanks bare", []string{"/usr/local/bin/thanks"}, []string{"/usr/local/bin/thanks", "room"}},
		{"thanks with args", []string{"thanks", "--room", "support", "hi"}, []string{"thanks", "room", "--room", "support", "hi"}},
		{"thanks help", []string{"thanks", "--help"}, []string{"thanks", "room", "--help"}},
		{"txco subcommand untouched", []string{"/opt/homebrew/bin/txco", "apply"}, []string{"/opt/homebrew/bin/txco", "apply"}},
		{"txco bare untouched", []string{"txco"}, []string{"txco"}},
		{"empty untouched", []string{}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := roomAlias(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("roomAlias(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestIMAPStoreMode(t *testing.T) {
	for _, c := range []struct {
		personalities, store string
		open, fatal          bool
	}{
		{"cron,web,admin", "sqlite", false, false},
		{"cron,web,admin", "", false, false},
		{"cron,web,admin,imap", "sqlite", true, true},
		{"cron,web,admin", "postgres", true, false},
		{"cron,web,imap", "postgres", true, true},
		{"cron,web,mailmap", "sqlite", false, false}, // "mailmap" is not "imap"
	} {
		open, fatal := imapStoreMode(c.personalities, c.store)
		if open != c.open || fatal != c.fatal {
			t.Errorf("imapStoreMode(%q, %q) = (%v, %v), want (%v, %v)", c.personalities, c.store, open, fatal, c.open, c.fatal)
		}
	}
}

func TestCalendarStoreMode(t *testing.T) {
	for _, c := range []struct {
		personalities, store string
		open, fatal          bool
	}{
		{"cron,web,admin", "sqlite", false, false},
		{"cron,web,admin", "", false, false},
		{"cron,web,admin,calendar", "sqlite", true, true},
		{"cron,web,admin", "postgres", true, false},
		{"cron,web,calendar", "postgres", true, true},
		{"cron,web,calendars", "sqlite", false, false}, // whole-token match
	} {
		open, fatal := calendarStoreMode(c.personalities, c.store)
		if open != c.open || fatal != c.fatal {
			t.Errorf("calendarStoreMode(%q, %q) = (%v, %v), want (%v, %v)", c.personalities, c.store, open, fatal, c.open, c.fatal)
		}
	}
}
