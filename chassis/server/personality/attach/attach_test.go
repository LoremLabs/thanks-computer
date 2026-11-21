package attach

import "testing"

// The web head asks this on every request. It must claim exactly the one
// hostname, whatever port the Host header carries, and nothing near it.
func TestMatchHost(t *testing.T) {
	m := MatchHost("attach.example.com")
	for host, want := range map[string]bool{
		"attach.example.com":       true,
		"attach.example.com:443":   true,
		"ATTACH.Example.COM":       true,
		"attach.example.com:8080":  true,
		"example.com":              false,
		"x.attach.example.com":     false,
		"attach.example.com.evil":  false,
		"notattach.example.com":    false,
		"attach.example.com.":      false,
		"":                         false,
		"localhost":                false,
		"attach.example.com@evil":  false,
		"evil.com/attach.example.": false,
	} {
		if got := m(host); got != want {
			t.Errorf("MatchHost(%q) = %v, want %v", host, got, want)
		}
	}
	// With nothing mounted, nothing matches — not even an empty Host.
	none := MatchHost("")
	for _, host := range []string{"", "attach.example.com", "localhost"} {
		if none(host) {
			t.Errorf("an unmounted head claimed %q", host)
		}
	}
}
