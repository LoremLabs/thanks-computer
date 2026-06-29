package cli

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// TestResolveTargetLabel: the confirm banner label reflects what resolveTarget
// actually used to pick the chassis URL — a positional profile name, not the
// default "dev" — so a prod push can't be mislabeled "dev".
func TestResolveTargetLabel(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	mp, err := auth.MetaPath("cloud")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveMeta(mp, auth.Meta{ChassisURL: "https://admin.thanks.computer", DefaultTenant: "prod-mankins"}); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir() // no txco.yaml

	cases := []struct {
		name                  string
		target, addr, profile string
		want                  string
	}{
		{"positional profile", "cloud", "", "", "cloud"},      // the bug: was "dev"
		{"--profile", "", "", "cloud", "cloud"},               // profile flag supplies the url
		{"raw --addr", "", "https://x:8081", "", ""},          // url stands alone
		{"raw url target", "https://x:8081", "", "", ""},      // ditto
		{"no profile → localhost default", "", "", "", "dev"}, // nothing supplied a url
	}
	for _, c := range cases {
		if got := resolveTargetLabel(ws, c.target, c.addr, c.profile); got != c.want {
			t.Errorf("%s: resolveTargetLabel(%q,%q,%q) = %q, want %q",
				c.name, c.target, c.addr, c.profile, got, c.want)
		}
	}
}
