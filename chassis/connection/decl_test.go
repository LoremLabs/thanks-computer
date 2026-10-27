package connection

import (
	"strings"
	"testing"
)

const goodProvider = `title: GitHub
kind: github_app
issuer: https://github.com
destinations:
  authorization: https://github.com/login/oauth/authorize
  token: https://github.com/login/oauth/access_token
  api: [https://api.github.com]
client_id: secret:GITHUB_APP_CLIENT_ID
client_secret: secret:GITHUB_APP_CLIENT_SECRET
pkce: true
account: {url: https://api.github.com/user, subject: id, name: login}
app:
  id: "12345"
  private_key: secret:GITHUB_APP_PRIVATE_KEY
consumers:
  tools-api: [api.github.read, api.github.write]
`

func TestParseProvider(t *testing.T) {
	p, err := ParseProvider([]byte(goodProvider))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindGitHubApp || p.ClientAuth != ClientAuthBody || p.Destinations.Revocation != "none" ||
		p.Refresh.Rotation != RotationSingleUse || p.Refresh.OnUncertain != OnUncertainReconnect ||
		p.Refresh.Margin.Fraction != DefaultMarginFraction || p.Refresh.Margin.FloorS != DefaultMarginFloorS || p.Refresh.Margin.CeilingS != DefaultMarginCeilingS {
		t.Errorf("defaults not applied: %+v", p)
	}
	if !p.Allows("tools-api", "api.github.read") || p.Allows("tools-api", "api.slack.read") || p.Allows("helper", "api.github.read") {
		t.Error("Allows: a stack and a capability are both named, no wildcards")
	}
	o, _ := ParseOrigin("https://api.github.com:443")
	if !p.APIOrigin(o) {
		t.Error("the default port is the declared origin")
	}
	if o, _ = ParseOrigin("https://api.github.com:8443"); p.APIOrigin(o) {
		t.Error("another port is another origin")
	}
	if name, ok := SecretName(p.ClientSecret); !ok || name != "GITHUB_APP_CLIENT_SECRET" {
		t.Errorf("SecretName = %q, %v", name, ok)
	}

	for name, edit := range map[string]func(string) string{
		"unknown key": func(s string) string { return s + "extra: 1\n" },
		"no kind":     func(s string) string { return strings.Replace(s, "kind: github_app\n", "", 1) },
		"bad kind":    func(s string) string { return strings.Replace(s, "kind: github_app", "kind: saml", 1) },
		"no token destination": func(s string) string {
			return strings.Replace(s, "  token: https://github.com/login/oauth/access_token\n", "", 1)
		},
		"http destination": func(s string) string {
			return strings.Replace(s, "token: https://github.com", "token: http://github.com", 1)
		},
		"no api": func(s string) string { return strings.Replace(s, "  api: [https://api.github.com]\n", "", 1) },
		"api with a path": func(s string) string {
			return strings.Replace(s, "[https://api.github.com]", "[https://api.github.com/v3]", 1)
		},
		"literal secret": func(s string) string {
			return strings.Replace(s, "client_secret: secret:GITHUB_APP_CLIENT_SECRET", "client_secret: hunter2", 1)
		},
		"bad secret ref": func(s string) string {
			return strings.Replace(s, "secret:GITHUB_APP_CLIENT_SECRET", "secret:not-a-name", 1)
		},
		"account off api": func(s string) string {
			return strings.Replace(s, "url: https://api.github.com/user", "url: https://github.com/user", 1)
		},
		"app without key": func(s string) string {
			return strings.Replace(s, "  private_key: secret:GITHUB_APP_PRIVATE_KEY\n", "", 1)
		},
		"app on oauth2": func(s string) string { return strings.Replace(s, "kind: github_app", "kind: oauth2", 1) },
		"consumer no caps": func(s string) string {
			return strings.Replace(s, "tools-api: [api.github.read, api.github.write]", "tools-api: []", 1)
		},
		"consumer bad cap": func(s string) string { return strings.Replace(s, "api.github.write", "api.*", 1) },
		"consumer twice":   func(s string) string { return strings.Replace(s, "api.github.write", "api.github.read", 1) },
		"bad rotation":     func(s string) string { return s + "refresh: {rotation: sometimes}\n" },
		"margin over half": func(s string) string { return s + "refresh: {margin: {fraction: 0.9}}\n" },
		"bad issuer": func(s string) string {
			return strings.Replace(s, "issuer: https://github.com", "issuer: github.com", 1)
		},
	} {
		if _, err := ParseProvider([]byte(edit(goodProvider))); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
	// A plain oauth2 provider needs no app and may use basic client auth.
	plain := strings.Replace(goodProvider, "kind: github_app", "kind: oauth2", 1)
	plain = strings.Replace(plain, "app:\n  id: \"12345\"\n  private_key: secret:GITHUB_APP_PRIVATE_KEY\n", "client_auth: basic\n", 1)
	if p, err := ParseProvider([]byte(plain)); err != nil || p.ClientAuth != ClientAuthBasic {
		t.Errorf("oauth2 with basic auth: %v", err)
	}
}

func TestParseConsumer(t *testing.T) {
	c, err := ParseConsumer([]byte("provider: github\norigins: [https://api.github.com]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CredentialOAuth2 || c.Use.Header != "authorization" || c.Use.Format != "Bearer {}" {
		t.Errorf("defaults: %+v", c)
	}
	o, _ := ParseOrigin("https://api.github.com")
	if !c.OriginAllowed(o) || len(c.OriginList()) != 1 {
		t.Error("origin not allowed")
	}
	c, err = ParseConsumer([]byte("provider: github\nkind: github_app_installation\norigins: [https://api.github.com:8443, http://localhost:9090]\nuse: {header: X-Token, format: 'token {}'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Use.Header != "x-token" || c.Kind != CredentialInstallation || len(c.OriginList()) != 2 {
		t.Errorf("parsed: %+v", c)
	}
	for name, body := range map[string]string{
		"no provider":    "origins: [https://api.github.com]\n",
		"bad provider":   "provider: GitHub\norigins: [https://api.github.com]\n",
		"no origins":     "provider: github\n",
		"http origin":    "provider: github\norigins: [http://api.github.com]\n",
		"bad kind":       "provider: github\nkind: password\norigins: [https://api.github.com]\n",
		"bad header":     "provider: github\norigins: [https://api.github.com]\nuse: {header: 'X Token'}\n",
		"format no slot": "provider: github\norigins: [https://api.github.com]\nuse: {format: 'Bearer'}\n",
		"unknown key":    "provider: github\norigins: [https://api.github.com]\nhosts: [x]\n",
	} {
		if _, err := ParseConsumer([]byte(body)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

func TestPaths(t *testing.T) {
	if ProviderName("PROVIDERS/github.yaml") != "github" || ConsumerName("CONNECTIONS/github.yaml") != "github" {
		t.Error("names")
	}
	for _, p := range []string{"PROVIDERS/GitHub.yaml", "PROVIDERS/a/github.yaml", "PROVIDERS/github.yml", "providers/github.yaml", "PROVIDERS/.yaml", "CONNECTIONS/github.yaml"} {
		if ProviderName(p) != "" {
			t.Errorf("ProviderName(%q) = %q, want none", p, ProviderName(p))
		}
	}
	if ProviderPath("github") != "PROVIDERS/github.yaml" || ConsumerPath("github") != "CONNECTIONS/github.yaml" {
		t.Error("paths")
	}
}
