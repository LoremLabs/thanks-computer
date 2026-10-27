package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

const testProviderYAML = `kind: oauth2
destinations:
  authorization: https://github.com/login/oauth/authorize
  token: https://github.com/login/oauth/access_token
  api: [https://api.github.com]
client_id: cid
client_secret: secret:GH_SECRET
consumers:
  loop: [api.github.read]
`

func TestValidateStackFilePathConnections(t *testing.T) {
	for _, p := range []string{"PROVIDERS/github.yaml", "CONNECTIONS/github.yaml"} {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"providers/github.yaml", "Connections/github.yaml"} {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want exact-case error", p)
		}
	}
}

func TestDeepValidateProvidersAndConnections(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	capsFixture(t, c)

	// The other active stack declares slack; a superseded draft of it and
	// another tenant's stack declare github, which must not collide.
	putCapFile(t, c, 10, "PROVIDERS/slack.yaml", strings.Replace(testProviderYAML, "github.com", "slack.com", -1), "")
	putCapFile(t, c, 11, "PROVIDERS/github.yaml", testProviderYAML, "")
	putCapFile(t, c, 20, "PROVIDERS/github.yaml", testProviderYAML, "")

	for p, body := range map[string]string{
		"PROVIDERS/github.yaml":   testProviderYAML,
		"PROVIDERS/slack.yaml":    strings.Replace(testProviderYAML, "github.com", "slack.com", -1),
		"PROVIDERS/broken.yaml":   "kind: oauth2\nextra: 1\n",
		"CONNECTIONS/github.yaml": "provider: github\norigins: [https://api.github.com]\n",
		"CONNECTIONS/slack.yaml":  "provider: slack\norigins: [https://api.slack.com, https://files.slack.com]\n",
		"CONNECTIONS/linear.yaml": "provider: linear\norigins: [https://api.linear.app]\n", // no provider in sight: fine
		"CONNECTIONS/wrong.yaml":  "provider: github\norigins: [https://api.github.com]\n",
		"CONNECTIONS/bad.yaml":    "provider: bad\n",
	} {
		putCapFile(t, c, 1, p, body, "")
	}

	issues := c.deepValidateProviders(ctx, "tnt_default", "stk_loop", 1)
	want := map[string]string{
		"PROVIDERS/slack.yaml":  `already declared by the active stack "other"`,
		"PROVIDERS/broken.yaml": "field extra not found",
	}
	if len(issues) != len(want) {
		t.Fatalf("providers: want %d issues, got %+v", len(want), issues)
	}
	for _, i := range issues {
		if sub, ok := want[i.Path]; !ok || !strings.Contains(i.Err, sub) {
			t.Errorf("unexpected provider issue %+v", i)
		}
	}

	issues = c.deepValidateConnections(ctx, "tnt_default", "stk_loop", 1)
	want = map[string]string{
		"CONNECTIONS/slack.yaml": "origin https://files.slack.com is not one of provider \"slack\"'s api destinations",
		"CONNECTIONS/wrong.yaml": `the file is for provider "wrong" but ` + "`provider`" + ` says "github"`,
		"CONNECTIONS/bad.yaml":   "origins: at least one origin is required",
	}
	if len(issues) != len(want) {
		t.Fatalf("connections: want %d issues, got %+v", len(want), issues)
	}
	for _, i := range issues {
		if sub, ok := want[i.Path]; !ok || !strings.Contains(i.Err, sub) {
			t.Errorf("unexpected connection issue %+v", i)
		}
	}

	// The stack's own active version re-declaring its provider is no collision.
	putCapFile(t, c, 11, "PROVIDERS/slack.yaml", strings.Replace(testProviderYAML, "github.com", "slack.com", -1), "")
	if issues := c.deepValidateProviders(ctx, "tnt_default", "stk_other", 11); len(issues) != 0 {
		t.Fatalf("own active version must not collide: %+v", issues)
	}
}
