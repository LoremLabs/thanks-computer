package cli

import (
	"reflect"
	"testing"
)

func TestTakeForceLogin(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		want  []string
		force bool
	}{
		{"absent", []string{"cloud"}, []string{"cloud"}, false},
		{"double dash", []string{"cloud", "--force-login"}, []string{"cloud"}, true},
		{"single dash", []string{"cloud", "-force-login"}, []string{"cloud"}, true},
		{"explicit true", []string{"--force-login=true", "cloud"}, []string{"cloud"}, true},
		{"explicit false", []string{"--force-login=false", "cloud"}, []string{"cloud"}, false},
		{"leading", []string{"--force-login", "cloud"}, []string{"cloud"}, true},
		{
			"preserves other flags",
			[]string{"cloud", "--force-login", "--tenant", "acme", "--no-open"},
			[]string{"cloud", "--tenant", "acme", "--no-open"},
			true,
		},
		// A bare positional that happens to spell the flag name is a profile
		// name, not our flag — only a dashed form is intercepted.
		{"bare positional", []string{"force-login"}, []string{"force-login"}, false},
		// A malformed bool is passed through for auth login's FlagSet to
		// reject, rather than being silently swallowed here.
		{"malformed value", []string{"--force-login=nope"}, []string{"--force-login=nope"}, false},
		{"empty", nil, []string{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, force := takeForceLogin(tc.args)
			if force != tc.force {
				t.Errorf("force = %v, want %v", force, tc.force)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("args = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// uiTargetProfile must agree with auth login's own resolution order, since
// they parse the same argv — a mismatch would run the OAuth handshake for one
// profile and then mint a session for a different one.
func TestUITargetProfile(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"none", nil, ""},
		{"positional", []string{"cloud"}, "cloud"},
		{"profile flag", []string{"--profile", "cloud-dev"}, "cloud-dev"},
		{"profile flag equals", []string{"--profile=cloud"}, "cloud"},
		{"target names profile", []string{"--target", "cloud"}, "cloud"},
		// A --target containing ":" is a raw admin URL, so it sets --url and
		// leaves the profile unresolved.
		{"target is a url", []string{"--target", "https://admin.example.com"}, ""},
		{"positional is a url", []string{"http://localhost:8081"}, ""},
		// --profile wins over a positional, matching applyTargetSelector's
		// "explicitly-passed --profile is left untouched" rule.
		{"flag beats positional", []string{"--profile", "cloud", "dev"}, "cloud"},
		// The positional follows flags that take values; `acme` is the
		// tenant's value, not the profile.
		{"value flag then positional", []string{"--tenant", "acme", "cloud"}, "cloud"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := uiTargetProfile(tc.args); got != tc.want {
				t.Errorf("uiTargetProfile(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestIsCloudProfile(t *testing.T) {
	for _, p := range []string{"", "cloud", "cloud-dev", "cloud-custom", "cloud-localhost-4200"} {
		if !isCloudProfile(p) {
			t.Errorf("isCloudProfile(%q) = false, want true", p)
		}
	}
	// Self-hosted profiles have no identity provider behind them; running
	// cloud login for one would enroll it against the prod cloud.
	for _, p := range []string{"local", "dev", "staging", "mycloud"} {
		if isCloudProfile(p) {
			t.Errorf("isCloudProfile(%q) = true, want false", p)
		}
	}
}
