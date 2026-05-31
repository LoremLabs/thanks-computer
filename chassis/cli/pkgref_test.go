package cli

import "testing"

func TestResolvePackageRef(t *testing.T) {
	none := registryConfig{}
	withAlias := registryConfig{Aliases: map[string]string{"ghcr": "ghcr.io/loremlabs"}}
	override := registryConfig{Default: "reg.example.com", DefaultNamespace: "acme"}

	cases := []struct {
		in   string
		reg  registryConfig
		want string
	}{
		// baked defaults
		{"sales@v3", none, "oci://registry.thanks.computer/txco/sales:v3"},
		{"sales", none, "oci://registry.thanks.computer/txco/sales"},
		{"acme/sales@v3", none, "oci://registry.thanks.computer/acme/sales:v3"},
		{"txco/customer-support/email@v3", none, "oci://registry.thanks.computer/txco/customer-support/email:v3"},
		// alias
		{"ghcr/foo@1.0", withAlias, "oci://ghcr.io/loremlabs/foo:1.0"},
		// workspace override
		{"sales@v3", override, "oci://reg.example.com/acme/sales:v3"},
		// explicit schemes pass through
		{"dir:./pkg", none, "dir:./pkg"},
		{"github:owner/repo/pkg", none, "github:owner/repo/pkg"},
		{"oci://host/ns/name:1", none, "oci://host/ns/name:1"},
		// local paths get a dir: prefix
		{"./examples/x", none, "dir:./examples/x"},
		{"/abs/x", none, "dir:/abs/x"},
	}
	for _, tc := range cases {
		if got := resolvePackageRef(tc.in, tc.reg); got != tc.want {
			t.Errorf("resolvePackageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
