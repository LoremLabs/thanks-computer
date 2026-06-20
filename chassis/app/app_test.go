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
