package opname

import "testing"

func TestCallTarget(t *testing.T) {
	for _, tc := range []struct {
		exec  string
		stack string
		scope int
		bad   bool
	}{
		{"call://billing", "billing", 0, false},
		{"call://app/_orient", "app/_orient", 0, false},
		{"call://app/onboard/150", "app/onboard", 150, false},
		{"call://billing/100", "billing", 100, false},
		// A last segment that is all digits is the scope, as for goto.
		{"call://2024/7", "2024", 7, false},
		{"call://", "", 0, true},
		{"call:///100", "", 0, true},
		{"call://bad name", "", 0, true},
		{"call://a//b", "", 0, true},
		{"goto://billing/100", "", 0, true},
	} {
		stack, scope, err := CallTarget(tc.exec)
		if tc.bad {
			if err == nil {
				t.Errorf("CallTarget(%q) = %q/%d, want an error", tc.exec, stack, scope)
			}
			continue
		}
		if err != nil {
			t.Errorf("CallTarget(%q): %v", tc.exec, err)
			continue
		}
		if stack != tc.stack || scope != tc.scope {
			t.Errorf("CallTarget(%q) = %q/%d, want %q/%d", tc.exec, stack, scope, tc.stack, tc.scope)
		}
	}
}
