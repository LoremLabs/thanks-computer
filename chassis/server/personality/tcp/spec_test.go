package tcp

import "testing"

func TestParseListenerSpecOptions(t *testing.T) {
	cases := []struct {
		in      string
		want    listenerSpec
		wantErr bool
	}{
		{":5050", listenerSpec{Name: "default", Addr: ":5050"}, false},
		{"irc=:6697;tls", listenerSpec{Name: "irc", Addr: ":6697", TLS: true}, false},
		{"irc=:6697; tls ;", listenerSpec{Name: "irc", Addr: ":6697", TLS: true}, false},
		{"dev=127.0.0.1:6697;self-signed", listenerSpec{Name: "dev", Addr: "127.0.0.1:6697", TLS: true, SelfSigned: true}, false},
		{"dev=:6697;tls;self-signed", listenerSpec{Name: "dev", Addr: ":6697", TLS: true, SelfSigned: true}, false},
		{"", listenerSpec{}, false},
		{"irc=:6697;tsl", listenerSpec{}, true},     // typo must not bind plaintext
		{"irc=:6697;proxy=x", listenerSpec{}, true}, // not until the edge phase lands
	}
	for _, tc := range cases {
		got, err := parseListenerSpec(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseListenerSpec(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseListenerSpec(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	specs, err := parseListenerSpecs([]string{"", "a=:1", "  ", "b=:2;tls"})
	if err != nil || len(specs) != 2 || !specs[1].TLS {
		t.Errorf("parseListenerSpecs: %+v err=%v", specs, err)
	}
}
