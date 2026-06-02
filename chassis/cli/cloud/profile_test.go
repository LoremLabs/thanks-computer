package cloud

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func TestDerivedCloudProfile(t *testing.T) {
	cases := map[string]string{
		"":                                "cloud", // prod default stays bare
		defaultCloudURL:                   "cloud",
		defaultCloudURL + "/":             "cloud",
		devCloudURL:                       "cloud-dev",
		"https://staging.thanks.computer": "cloud-staging-thanks-computer",
		"http://10.0.0.5:8080":            "cloud-10-0-0-5-8080",
	}
	for in, want := range cases {
		if got := derivedCloudProfile(in); got != want {
			t.Errorf("derivedCloudProfile(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloudProfileWrite(t *testing.T) {
	t.Setenv("TXCO_PROFILE", "")
	if got := cloudProfile("flagval", defaultCloudURL); got != "flagval" {
		t.Fatalf("flag: got %q, want flagval", got)
	}
	t.Setenv("TXCO_PROFILE", "fromenv")
	if got := cloudProfile("", devCloudURL); got != "fromenv" {
		t.Fatalf("env: got %q, want fromenv", got)
	}
	t.Setenv("TXCO_PROFILE", "")
	if got := cloudProfile("", devCloudURL); got != "cloud-dev" {
		t.Fatalf("derive: got %q, want cloud-dev", got)
	}
}

func TestResolveCloudBase(t *testing.T) {
	if got := resolveCloudBase("https://x.example", false); got != "https://x.example" {
		t.Fatalf("flag: %q", got)
	}
	if got := resolveCloudBase("", true); got != devCloudURL {
		t.Fatalf("dev: %q", got)
	}
	if got := resolveCloudBase("", false); got != defaultCloudURL {
		t.Fatalf("default: %q", got)
	}
}

func TestResolveCloudReadProfile(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_PROFILE", "")

	// No tokens, no active profile → prod-derived default.
	if got := resolveCloudReadProfile(""); got != "cloud" {
		t.Fatalf("empty: got %q, want cloud", got)
	}
	// Explicit flag always wins.
	if got := resolveCloudReadProfile("explicit"); got != "explicit" {
		t.Fatalf("flag: got %q, want explicit", got)
	}
	// Exactly one token → that profile, no flag needed.
	if err := SaveCloudToken("cloud-dev", CloudToken{Kind: "cloud"}); err != nil {
		t.Fatalf("SaveCloudToken: %v", err)
	}
	if got := resolveCloudReadProfile(""); got != "cloud-dev" {
		t.Fatalf("sole token: got %q, want cloud-dev", got)
	}
	// A second token makes "sole" ambiguous; the active profile (with a token)
	// wins.
	if err := SaveCloudToken("cloud", CloudToken{Kind: "cloud"}); err != nil {
		t.Fatalf("SaveCloudToken: %v", err)
	}
	if err := auth.WriteActiveProfile("cloud-dev"); err != nil {
		t.Fatalf("WriteActiveProfile: %v", err)
	}
	if got := resolveCloudReadProfile(""); got != "cloud-dev" {
		t.Fatalf("follow active: got %q, want cloud-dev", got)
	}
}

func TestEnrollDegradeMessageShowsEndpoint(t *testing.T) {
	ep := "https://admin.thanks.computer/auth/oauth/enroll"
	msg404 := enrollDegradeMessage(&client.HTTPError{StatusCode: http.StatusNotFound}, ep)
	if !strings.Contains(msg404, ep) || !strings.Contains(msg404, "404") {
		t.Fatalf("404 message missing endpoint/404: %q", msg404)
	}
	msgGeneric := enrollDegradeMessage(errors.New("boom"), ep)
	if !strings.Contains(msgGeneric, ep) {
		t.Fatalf("generic message missing endpoint: %q", msgGeneric)
	}
}

func TestSameChassisHost(t *testing.T) {
	cases := []struct {
		meta, endpoint string
		want           bool
	}{
		{"https://admin.thanks.computer", "https://admin.thanks.computer/auth/oauth/enroll", true},
		{"http://localhost:8088", "https://admin.thanks.computer/auth/oauth/enroll", false},
		{"http://localhost:8081", "http://localhost:8081/auth/oauth/enroll", true},
		{"", "https://admin.thanks.computer/auth/oauth/enroll", false},
	}
	for _, c := range cases {
		if got := sameChassisHost(c.meta, c.endpoint); got != c.want {
			t.Errorf("sameChassisHost(%q,%q) = %v, want %v", c.meta, c.endpoint, got, c.want)
		}
	}
}
