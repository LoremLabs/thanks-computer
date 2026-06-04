package cli

import (
	"reflect"
	"testing"
)

func TestSplitForwardFlags(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantGlobals map[string]string
		wantRest    []string
	}{
		{
			name:        "profile space form extracted, positionals kept",
			args:        []string{"grant", "show", "prod-mankins", "--profile", "txco-production"},
			wantGlobals: map[string]string{"profile": "txco-production"},
			wantRest:    []string{"grant", "show", "prod-mankins"},
		},
		{
			name:        "profile equals form",
			args:        []string{"grant", "show", "x", "--profile=prod"},
			wantGlobals: map[string]string{"profile": "prod"},
			wantRest:    []string{"grant", "show", "x"},
		},
		{
			name:        "global flag in the middle",
			args:        []string{"grant", "--addr", "https://h:1", "show", "x"},
			wantGlobals: map[string]string{"addr": "https://h:1"},
			wantRest:    []string{"grant", "show", "x"},
		},
		{
			name:        "command-specific flags are preserved verbatim",
			args:        []string{"foo", "--bar", "baz", "--profile", "p"},
			wantGlobals: map[string]string{"profile": "p"},
			wantRest:    []string{"foo", "--bar", "baz"},
		},
		{
			name:        "no flags",
			args:        []string{"grant", "show", "x"},
			wantGlobals: map[string]string{},
			wantRest:    []string{"grant", "show", "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, r := splitForwardFlags(tc.args)
			if !reflect.DeepEqual(g, tc.wantGlobals) {
				t.Errorf("globals = %v, want %v", g, tc.wantGlobals)
			}
			if !reflect.DeepEqual(r, tc.wantRest) {
				t.Errorf("rest = %v, want %v", r, tc.wantRest)
			}
		})
	}
}
