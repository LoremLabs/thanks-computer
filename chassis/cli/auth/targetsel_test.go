package auth

import "testing"

func TestApplyTargetSelector(t *testing.T) {
	// A URL value → folded into url (profile untouched).
	u, p := "", ""
	applyTargetSelector("https://x:8081", &u, &p)
	if u != "https://x:8081" || p != "" {
		t.Errorf("url: got url=%q profile=%q", u, p)
	}

	// host:port (no scheme) is also a URL.
	u, p = "", ""
	applyTargetSelector("localhost:9", &u, &p)
	if u != "localhost:9" || p != "" {
		t.Errorf("host:port: got url=%q profile=%q", u, p)
	}

	// A bare name → folded into profile.
	u, p = "", ""
	applyTargetSelector("cloud", &u, &p)
	if p != "cloud" || u != "" {
		t.Errorf("name: got url=%q profile=%q", u, p)
	}

	// Explicit --url / --profile are NOT overridden by --target.
	u, p = "http://set", "myprof"
	applyTargetSelector("cloud", &u, &p)
	if u != "http://set" || p != "myprof" {
		t.Errorf("explicit preserved: got url=%q profile=%q", u, p)
	}

	// Empty --target is a no-op.
	u, p = "", ""
	applyTargetSelector("", &u, &p)
	if u != "" || p != "" {
		t.Errorf("empty noop: got url=%q profile=%q", u, p)
	}
}

func TestApplyTargetSelectorName(t *testing.T) {
	// A name overrides the command's non-empty default key name.
	u, n := "", "local"
	applyTargetSelectorName("cloud", &u, &n)
	if n != "cloud" || u != "" {
		t.Errorf("name override: got url=%q name=%q", u, n)
	}

	// A URL → url; the default name is left alone.
	u, n = "", "local"
	applyTargetSelectorName("https://x", &u, &n)
	if u != "https://x" || n != "local" {
		t.Errorf("url: got url=%q name=%q", u, n)
	}
}
