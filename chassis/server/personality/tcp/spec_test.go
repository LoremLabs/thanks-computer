package tcp

import (
	"fmt"
	"net"
	"testing"
)

func cidr(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestParseListenerSpecOptions(t *testing.T) {
	cases := []struct {
		in      string
		want    listenerSpec
		wantErr bool
	}{
		{":5050", listenerSpec{Name: "default", Addr: ":5050", Handler: "line"}, false},
		{"irc=:6697;tls", listenerSpec{Name: "irc", Addr: ":6697", TLS: true, Handler: "line"}, false},
		{"irc=:6697; tls ;", listenerSpec{Name: "irc", Addr: ":6697", TLS: true, Handler: "line"}, false},
		{"dev=127.0.0.1:6697;self-signed", listenerSpec{Name: "dev", Addr: "127.0.0.1:6697", TLS: true, SelfSigned: true, Handler: "line"}, false},
		{"dev=:6697;tls;self-signed", listenerSpec{Name: "dev", Addr: ":6697", TLS: true, SelfSigned: true, Handler: "line"}, false},
		{"", listenerSpec{}, false},
		{"edge=:16697;proxy=172.16.0.0/12|fdaa::/8", listenerSpec{Name: "edge", Addr: ":16697",
			Proxy: []*net.IPNet{cidr(t, "172.16.0.0/12"), cidr(t, "fdaa::/8")}, Handler: "line"}, false},
		{"edge=:16697;proxy=127.0.0.1", listenerSpec{Name: "edge", Addr: ":16697",
			Proxy: []*net.IPNet{cidr(t, "127.0.0.1/32")}, Handler: "line"}, false}, // a bare IP is a /32
		{"echo=:7;handler=echo", listenerSpec{Name: "echo", Addr: ":7", Handler: "echo"}, false},
		{"e=:7;self-signed;handler=echo", listenerSpec{Name: "e", Addr: ":7", TLS: true, SelfSigned: true, Handler: "echo"}, false},
		{"irc=:6697;handler=irc", listenerSpec{}, true},                  // not in this build: refuse, never fall back to line
		{"irc=:6697;handler=", listenerSpec{}, true},                     //
		{"irc=:6697;tsl", listenerSpec{}, true},                          // typo must not bind plaintext
		{"irc=:6697;proxy=x", listenerSpec{}, true},                      // nor trust nobody-in-particular
		{"irc=:6697;proxy=", listenerSpec{}, true},                       //
		{"irc=:6697;proxy=10.0.0.0/8|nope", listenerSpec{}, true},        //
		{"irc=:6697;tls;proxy=10.0.0.0/8", listenerSpec{}, true},         // one TLS terminator per door
		{"irc=:6697;proxy=10.0.0.0/8;self-signed", listenerSpec{}, true}, //
	}
	for _, tc := range cases {
		got, err := parseListenerSpec(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseListenerSpec(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		// %+v, not DeepEqual: a parsed bare IP is 16 bytes, ParseCIDR's is 4.
		if err == nil && fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tc.want) {
			t.Errorf("parseListenerSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	specs, err := parseListenerSpecs([]string{"", "a=:1", "  ", "b=:2;tls"})
	if err != nil || len(specs) != 2 || !specs[1].TLS {
		t.Errorf("parseListenerSpecs: %+v err=%v", specs, err)
	}
}
