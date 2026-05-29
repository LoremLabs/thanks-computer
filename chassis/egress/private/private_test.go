package private_test

import (
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
)

func TestPrivateBlocksAndAllows(t *testing.T) {
	g, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatalf("Open private: %v", err)
	}
	if g.Name() != "private" {
		t.Fatalf("Name = %q, want private", g.Name())
	}

	cases := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1:80", true},
		{"10.1.2.3:443", true},
		{"192.168.1.1:80", true},
		{"172.16.0.1:80", true},
		{"172.31.255.255:80", true},
		{"169.254.169.254:80", true}, // cloud metadata
		{"100.64.0.1:80", true},      // CGNAT / Tailscale
		{"0.0.0.0:80", true},
		{"[::1]:80", true},
		{"[fe80::1]:80", true},
		{"[fc00::1]:80", true},
		{"[fd12:3456::1]:80", true},
		{"[::ffff:127.0.0.1]:80", true},  // IPv4-mapped loopback
		{"[2002:7f00:1::1]:80", true},    // 6to4 wrapping 127.0.0.1
		{"[64:ff9b::a01:101]:80", true},  // NAT64 wrapping 10.1.1.1
		{"1.1.1.1:443", false},
		{"8.8.8.8:53", false},
		{"[2606:4700:4700::1111]:443", false},
	}
	for _, c := range cases {
		err := g.CheckAddr("tcp", c.addr)
		if c.blocked && err == nil {
			t.Errorf("%s: expected blocked, got allowed", c.addr)
		}
		if !c.blocked && err != nil {
			t.Errorf("%s: expected allowed, got %v", c.addr, err)
		}
	}
}

func TestAllowOverridesDeny(t *testing.T) {
	g, err := egress.Open("private", egress.Config{
		AllowCIDRs: []string{"10.9.0.0/16"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := g.CheckAddr("tcp", "10.9.1.1:80"); err != nil {
		t.Errorf("allow-listed 10.9.1.1 should be permitted, got %v", err)
	}
	if err := g.CheckAddr("tcp", "10.1.1.1:80"); err == nil {
		t.Errorf("10.1.1.1 not in allow list should still be blocked")
	}
}

func TestExtraDenyCIDR(t *testing.T) {
	g, err := egress.Open("private", egress.Config{
		DenyCIDRs: []string{"203.0.50.0/24"},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := g.CheckAddr("tcp", "203.0.50.7:80"); err == nil {
		t.Errorf("operator deny CIDR 203.0.50.0/24 should block 203.0.50.7")
	}
	if err := g.CheckAddr("tcp", "1.1.1.1:80"); err != nil {
		t.Errorf("public addr should still be allowed, got %v", err)
	}
}

func TestMalformedCIDRFailsOpen(t *testing.T) {
	if _, err := egress.Open("private", egress.Config{DenyCIDRs: []string{"not-a-cidr"}}); err == nil {
		t.Fatalf("expected Open error for malformed deny CIDR")
	}
	if _, err := egress.Open("private", egress.Config{AllowCIDRs: []string{"10.0.0.0/99"}}); err == nil {
		t.Fatalf("expected Open error for malformed allow CIDR")
	}
}

func TestOpenAllowsEverything(t *testing.T) {
	g, err := egress.Open("open", egress.Config{})
	if err != nil {
		t.Fatalf("Open open: %v", err)
	}
	if g.Name() != "open" {
		t.Fatalf("Name = %q, want open", g.Name())
	}
	for _, a := range []string{"127.0.0.1:80", "169.254.169.254:80", "1.1.1.1:443"} {
		if err := g.CheckAddr("tcp", a); err != nil {
			t.Errorf("open policy should allow %s, got %v", a, err)
		}
	}
}

func TestUnknownPolicy(t *testing.T) {
	_, err := egress.Open("nope", egress.Config{})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("expected unknown policy error, got %v", err)
	}
}
