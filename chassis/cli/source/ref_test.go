package source

import "testing"

func TestParseRef(t *testing.T) {
	cases := []struct {
		in                         string
		reg, ns, name, tag, digest string
		err                        bool
	}{
		{in: "oci://registry.thanks.computer/txco/sales:v3", reg: "registry.thanks.computer", ns: "txco", name: "sales", tag: "v3"},
		{in: "registry.thanks.computer/txco/sales:v3", reg: "registry.thanks.computer", ns: "txco", name: "sales", tag: "v3"},
		{in: "ghcr.io/loremlabs/support-basic:0.1.0", reg: "ghcr.io", ns: "loremlabs", name: "support-basic", tag: "0.1.0"},
		{in: "localhost:5000/txco/x:0.1.0", reg: "localhost:5000", ns: "txco", name: "x", tag: "0.1.0"},
		{in: "host/txco/customer-support/email:v3", reg: "host", ns: "txco/customer-support", name: "email", tag: "v3"},
		{in: "host/ns/name", reg: "host", ns: "ns", name: "name", tag: "latest"},
		{in: "host/name@sha256:abc123", reg: "host", ns: "", name: "name", digest: "sha256:abc123"},
		{in: "oci://host/ns/name@sha256:deadbeef", reg: "host", ns: "ns", name: "name", digest: "sha256:deadbeef"},
		{in: "nohost", err: true},
		{in: "host/name@sha256:", err: true},
		{in: "host/name@badformat", err: true},
		{in: "", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			r, err := ParseRef(tc.in)
			if tc.err {
				if err == nil {
					t.Errorf("expected error for %q, got %+v", tc.in, r)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q): %v", tc.in, err)
			}
			if r.Registry != tc.reg || r.Namespace != tc.ns || r.Name != tc.name || r.Tag != tc.tag || r.Digest != tc.digest {
				t.Errorf("ParseRef(%q) = %+v, want reg=%s ns=%s name=%s tag=%s digest=%s",
					tc.in, r, tc.reg, tc.ns, tc.name, tc.tag, tc.digest)
			}
		})
	}
}

func TestParsedRefMethods(t *testing.T) {
	r, _ := ParseRef("registry.thanks.computer/txco/sales:v3")
	if got := r.Repository(); got != "registry.thanks.computer/txco/sales" {
		t.Errorf("Repository = %q", got)
	}
	if got := r.TagOrDigest(); got != "v3" {
		t.Errorf("TagOrDigest = %q", got)
	}
	if got := r.Reference(); got != "registry.thanks.computer/txco/sales:v3" {
		t.Errorf("Reference = %q", got)
	}
	if got := r.WithDigest("sha256:abc"); got != "registry.thanks.computer/txco/sales@sha256:abc" {
		t.Errorf("WithDigest = %q", got)
	}

	d, _ := ParseRef("host/name@sha256:abc")
	if got := d.TagOrDigest(); got != "sha256:abc" {
		t.Errorf("digest TagOrDigest = %q", got)
	}
	if got := d.Reference(); got != "host/name@sha256:abc" {
		t.Errorf("digest Reference = %q", got)
	}
}
