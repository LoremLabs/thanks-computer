package edgeproxy

import (
	"net/http"
	"strings"
	"testing"
)

func request(peer string, xff ...string) *http.Request {
	r, _ := http.NewRequest("GET", "http://example.test/", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// The shape production sends: the edge pins the client, then each internal
// hop appends itself (captured from a live request, 2026-09-19).
const flyRanges = "172.16.0.0/12 fdaa::/8"

func TestClientIP(t *testing.T) {
	trusting, bad := NewClients([]string{flyRanges})
	if len(bad) != 0 || !trusting.Trusts() {
		t.Fatalf("NewClients(%q): bad=%v trusts=%v", flyRanges, bad, trusting.Trusts())
	}
	for _, tc := range []struct {
		name string
		c    *Clients
		peer string
		xff  []string
		want string
	}{
		{"no trust list: the peer, header unread", nil, "198.51.100.4:4012", []string{"203.0.113.7"}, "198.51.100.4"},
		{"zero value trusts nobody", &Clients{}, "172.16.25.74:4012", []string{"203.0.113.7"}, "172.16.25.74"},
		{"peer outside the list: its header is just input", trusting, "198.51.100.4:4012", []string{"203.0.113.7"}, "198.51.100.4"},
		{"production: client, edge, load balancer", trusting, "[fdaa:98:7714:0:1::3]:51820",
			[]string{"176.151.108.50, fdaa:98:7714:a7b:5b5:6545:b92d:2, fdaa:98:7714:0:1::2"}, "176.151.108.50"},
		{"ipv4 peer in the list", trusting, "172.16.25.74:4012", []string{"176.151.108.50, 172.16.41.34"}, "176.151.108.50"},
		{"a forged entry sits left of the real one and is never reached", trusting, "172.16.25.74:4012",
			[]string{"10.0.0.1, 1.1.1.1, 176.151.108.50, fdaa::2"}, "176.151.108.50"},
		{"a forged TRUSTED-looking entry cannot hide the client", trusting, "172.16.25.74:4012",
			[]string{"172.16.0.9, 176.151.108.50, fdaa::2"}, "176.151.108.50"},
		{"several header lines read as one list", trusting, "172.16.25.74:4012",
			[]string{"10.0.0.1", "176.151.108.50", "fdaa::2"}, "176.151.108.50"},
		{"no header: the peer", trusting, "172.16.25.74:4012", nil, "172.16.25.74"},
		{"empty header: the peer", trusting, "172.16.25.74:4012", []string{""}, "172.16.25.74"},
		{"an unreadable hop stops the walk at the peer", trusting, "172.16.25.74:4012",
			[]string{"203.0.113.7, unknown, fdaa::2"}, "172.16.25.74"},
		{"a request that began inside: the hop that began it", trusting, "172.16.25.74:4012",
			[]string{"fdaa::77, fdaa::2"}, "fdaa::77"},
		{"hops with ports and brackets", trusting, "172.16.25.74:4012",
			[]string{"[2001:db8::5]:4433, 172.16.0.2:80"}, "2001:db8::5"},
		{"a bare ipv6 client", trusting, "172.16.25.74:4012", []string{"2001:db8::5, fdaa::2"}, "2001:db8::5"},
		{"ipv4-mapped ipv6 is the ipv4 address", trusting, "[::ffff:172.16.25.74]:4012",
			[]string{"::ffff:203.0.113.7"}, "203.0.113.7"},
		{"a zoned peer", nil, "[fe80::1%en0]:4012", nil, "fe80::1"},
		{"a peer without a port", nil, "198.51.100.4", nil, "198.51.100.4"},
		{"a unix-socket peer is handed back as it came", trusting, "@", []string{"203.0.113.7"}, "@"},
	} {
		if got := tc.c.IP(request(tc.peer, tc.xff...)); got != tc.want {
			t.Errorf("%s: IP = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// One environment variable holds the whole list, and how it was split
// depends on who wrote it. "a,b" as ONE entry used to parse as nothing.
func TestParseTrustedSplitsEntries(t *testing.T) {
	for _, entries := range [][]string{
		{"172.16.0.0/12", "fdaa::/8"},
		{"172.16.0.0/12 fdaa::/8"},
		{"172.16.0.0/12,fdaa::/8"},
		{" 172.16.0.0/12 ,\tfdaa::/8 ", ""},
	} {
		nets, bad := ParseTrusted(entries)
		if len(nets) != 2 || len(bad) != 0 {
			t.Errorf("ParseTrusted(%q) = %d nets, bad %v; want 2 nets", entries, len(nets), bad)
		}
	}
	c, _ := NewClients([]string{"172.16.0.0/12, nonsense 10.0.0.1"})
	if got := strings.Join(c.Networks(), " "); got != "172.16.0.0/12 10.0.0.1/32" {
		t.Errorf("Networks = %q, want the two that parsed", got)
	}
	if (*Clients)(nil).Networks() != nil {
		t.Error("nil Clients lists networks")
	}
	nets, bad := ParseTrusted([]string{"172.16.0.0/12, nonsense 10.0.0.1"})
	if len(nets) != 2 || len(bad) != 1 || bad[0] != "nonsense" {
		t.Errorf("mixed entry: %d nets, bad %v; want 2 nets and [nonsense]", len(nets), bad)
	}
}
