package auth

import (
	"bytes"
	"strings"
	"testing"
)

// The policy command refuses a bad request before it looks for a chassis.
func TestSecretsPolicyArguments(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no name":              {[]string{"--pull", "any"}, "NAME is required"},
		"no policy":            {[]string{"DB_DSN"}, "--pull is required"},
		"an unknown policy":    {[]string{"DB_DSN", "--pull", "all"}, `--pull "all"`},
		"a policy in capitals": {[]string{"DB_DSN", "--pull", "ANY"}, `--pull "ANY"`},
		"the policy first":     {[]string{"--pull", "everything", "DB_DSN"}, `--pull "everything"`},
	} {
		var stdout, stderr bytes.Buffer
		if code := runTenantSecrets(append([]string{"policy"}, tc.args...), &stdout, &stderr); code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %q)", name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%s: stderr %q does not say %q", name, stderr.String(), tc.want)
		}
	}

	var stdout, stderr bytes.Buffer
	runTenantSecrets([]string{"help"}, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "policy NAME --pull none|reviewed|any") {
		t.Errorf("usage does not list the policy command:\n%s", stdout.String())
	}
	if got := pullOrNone(""); got != "none" {
		t.Errorf("a chassis that sends no policy reads as %q", got)
	}
}
