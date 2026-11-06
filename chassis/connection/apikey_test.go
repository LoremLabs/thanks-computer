package connection

import (
	"context"
	"net/url"
	"strings"
	"testing"
)

const testKey = "ctx7sk-0123456789abcdef"

// keyRig is the service rig with the template provider a stack would declare
// for tool servers, and tools-mcp as its consumer.
func keyRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	p, err := ParseProvider([]byte("title: Tool servers\nkind: api_key\ninstances: true\nconsumers:\n  tools-mcp: [mcp.call]\n"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConsumer([]byte("provider: mcp\nkind: api_key\nuse: {header: authorization, format: \"Bearer {}\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.decls.keys = map[string]*Provider{"mcp": p}
	r.decls.consumers["tools-mcp/mcp"] = c
	return r
}

func (r *rig) putKey(principal, provider, addr, key string) (Row, *Error) {
	return r.svc.PutKey(r.ctx, PutKeyInput{
		Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: principal,
		Provider: provider, URL: addr, Key: []byte(key),
	})
}

func (r *rig) useKey(principal, provider, addr string) (UseResult, *Error) {
	u, err := url.Parse(addr)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.svc.Use(r.ctx, UseInput{Tenant: "acme", Stack: "tools-mcp", Provider: provider, Principal: principal, Capability: "mcp.call", URL: u})
}

func TestPastedKeyIsUsedAtItsOwnAddress(t *testing.T) {
	r := keyRig(t)
	addr := r.fp.srv.URL + "/mcp"
	row, cerr := r.putKey("service:pony-a", "mcp:ctx", addr, testKey)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if row.Kind != CredentialAPIKey || row.Account != r.fp.srv.URL || row.Status != StatusActive || row.ExpiresAt != nil {
		t.Fatalf("row = %+v", row)
	}
	res, cerr := r.useKey("service:pony-a", "mcp:ctx", addr+"?x=1")
	if cerr != nil {
		t.Fatal(cerr)
	}
	if res.Value != "Bearer "+testKey {
		t.Fatalf("value = %q", res.Value)
	}
	// Another path on the same origin is the same server's business; another
	// port or host is not.
	for _, other := range []string{"http://127.0.0.2:1/mcp", "https://evil.example/mcp"} {
		if _, cerr := r.useKey("service:pony-a", "mcp:ctx", other); cerr == nil || cerr.Code != CodeReconnect {
			t.Fatalf("%s: got %v, want %s", other, cerr, CodeReconnect)
		} else if strings.Contains(cerr.Message, testKey) {
			t.Fatal("the refusal quotes the key")
		}
	}
}

func TestPastedKeyIsSealed(t *testing.T) {
	r := keyRig(t)
	row, cerr := r.putKey("service:pony-a", "mcp:ctx", r.fp.srv.URL+"/mcp", testKey)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if strings.Contains(row.String(), testKey) {
		t.Fatal("a row's text form holds the key")
	}
	var material string
	if err := r.store.DB.QueryRow(`SELECT material FROM connections WHERE id = ?`, row.ID).Scan(&material); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(material, testKey) {
		t.Fatal("the key is stored in the clear")
	}
}

func TestPastedKeyBelongsToOnePrincipalAndInstance(t *testing.T) {
	r := keyRig(t)
	addr := r.fp.srv.URL + "/mcp"
	if _, cerr := r.putKey("service:pony-a", "mcp:ctx", addr, testKey); cerr != nil {
		t.Fatal(cerr)
	}
	if _, cerr := r.useKey("service:pony-b", "mcp:ctx", addr); cerr == nil || cerr.Code != CodeMissing {
		t.Fatalf("another pony: %v", cerr)
	}
	if _, cerr := r.useKey("service:pony-a", "mcp:other", addr); cerr == nil || cerr.Code != CodeMissing {
		t.Fatalf("another server: %v", cerr)
	}
}

func TestPastedKeyChecksTheConsumer(t *testing.T) {
	r := keyRig(t)
	addr := r.fp.srv.URL + "/mcp"
	if _, cerr := r.putKey("service:pony-a", "mcp:ctx", addr, testKey); cerr != nil {
		t.Fatal(cerr)
	}
	u, _ := url.Parse(addr)
	for name, in := range map[string]UseInput{
		"a stack the provider does not name": {Stack: "helper", Principal: "service:pony-a", Capability: "mcp.call"},
		"a capability it does not name":      {Stack: "tools-mcp", Principal: "service:pony-a", Capability: "mail.send"},
		"outside a capability call":          {Stack: "tools-mcp", Principal: "service:pony-a"},
	} {
		in.Tenant, in.Provider, in.URL = "acme", "mcp:ctx", u
		if _, cerr := r.svc.Use(r.ctx, in); cerr == nil || cerr.Code != CodeDenied {
			t.Errorf("%s: %v", name, cerr)
		}
	}
	if _, cerr := r.useKey("service:pony-a", "mcp", addr); cerr == nil || cerr.Code != CodeDenied {
		t.Fatalf("the template itself is no connection: %v", cerr)
	}
}

func TestPastingAgainReplacesTheKeyAndItsAddress(t *testing.T) {
	r := keyRig(t)
	old, moved := r.fp.srv.URL+"/mcp", "https://mcp.example.com/mcp"
	first, cerr := r.putKey("service:pony-a", "mcp:ctx", old, testKey)
	if cerr != nil {
		t.Fatal(cerr)
	}
	second, cerr := r.putKey("service:pony-a", "mcp:ctx", moved, testKey+"-2")
	if cerr != nil {
		t.Fatal(cerr)
	}
	if second.ID != first.ID || second.Generation != first.Generation+1 {
		t.Fatalf("first %+v second %+v", first, second)
	}
	res, cerr := r.useKey("service:pony-a", "mcp:ctx", moved)
	if cerr != nil || res.Token != testKey+"-2" {
		t.Fatalf("token %q err %v", res.Token, cerr)
	}
	if _, cerr := r.useKey("service:pony-a", "mcp:ctx", old); cerr == nil {
		t.Fatal("the old address still gets a key")
	}
}

func TestRevokingAPastedKeyStopsIt(t *testing.T) {
	r := keyRig(t)
	addr := r.fp.srv.URL + "/mcp"
	if _, cerr := r.putKey("service:pony-a", "mcp:ctx", addr, testKey); cerr != nil {
		t.Fatal(cerr)
	}
	calls := r.fp.tokens.Load()
	if _, removed, cerr := r.svc.Revoke(r.ctx, "acme", "tnt_acme", "service:pony-a", "mcp:ctx"); cerr != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, cerr)
	}
	if r.fp.tokens.Load() != calls {
		t.Fatal("revoking a key told a token destination")
	}
	if _, cerr := r.useKey("service:pony-a", "mcp:ctx", addr); cerr == nil || cerr.Code != CodeMissing {
		t.Fatalf("after revoke: %v", cerr)
	}
}

func TestPutKeyRefusals(t *testing.T) {
	r := keyRig(t)
	addr := r.fp.srv.URL + "/mcp"
	for name, c := range map[string]struct {
		provider, addr, key, code string
	}{
		"no such provider":        {"nope:x", addr, testKey, CodeNotDeclared},
		"a sign-in provider":      {"github", addr, testKey, CodeUnsupported},
		"no instance":             {"mcp", addr, testKey, CodeInvalidArg},
		"a bad name":              {"mcp:../x", addr, testKey, CodeInvalidArg},
		"plain http to a host":    {"mcp:x", "http://mcp.example.com/mcp", testKey, CodeDestination},
		"a user in the address":   {"mcp:x", "https://u:p@mcp.example.com/mcp", testKey, CodeDestination},
		"no address":              {"mcp:x", "", testKey, CodeDestination},
		"an empty key":            {"mcp:x", addr, "", CodeInvalidArg},
		"a space":                 {"mcp:x", addr, "two words", CodeInvalidArg},
		"a line break":            {"mcp:x", addr, "key\r\nx-evil: 1", CodeInvalidArg},
		"a word that is no ascii": {"mcp:x", addr, "kéy", CodeInvalidArg},
		"a pasted page":           {"mcp:x", addr, strings.Repeat("k", MaxKeyBytes+1), CodeInvalidArg},
	} {
		if _, cerr := r.putKey("service:pony-a", c.provider, c.addr, c.key); cerr == nil || cerr.Code != c.code {
			t.Errorf("%s: got %v, want %s", name, cerr, c.code)
		} else if c.key != "" && strings.Contains(cerr.Message, c.key) {
			t.Errorf("%s: the refusal quotes the key", name)
		}
	}
}

func TestStartRefusesAPastedKeyProvider(t *testing.T) {
	r := keyRig(t)
	_, cerr := r.svc.Start(r.ctx, StartInput{
		Tenant: "acme", TenantID: "tnt_acme", Stack: "loop", Principal: "service:pony-a", Provider: "mcp",
		RedirectURI: "https://h.example/connect/mcp/callback", Host: "h.example",
	})
	if cerr == nil || cerr.Code != CodeUnsupported {
		t.Fatalf("got %v", cerr)
	}
}

func TestFixedOriginKeyNeedsItsOriginDeclaredTwice(t *testing.T) {
	r := keyRig(t)
	p, err := ParseProvider([]byte("kind: api_key\ndestinations: {api: [" + r.fp.srv.URL + "]}\nconsumers:\n  tools-mcp: [mcp.call]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.decls.keys["fixed"] = p
	if _, cerr := r.putKey("service:pony-a", "fixed", "", testKey); cerr != nil {
		t.Fatal(cerr)
	}
	// A consumer naming no origin sends nothing.
	c, err := ParseConsumer([]byte("provider: fixed\nkind: api_key\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.decls.consumers["tools-mcp/fixed"] = c
	if _, cerr := r.useKey("service:pony-a", "fixed", r.fp.srv.URL); cerr == nil || cerr.Code != CodeDestination {
		t.Fatalf("no consumer origin: %v", cerr)
	}
	c, err = ParseConsumer([]byte("provider: fixed\nkind: api_key\norigins: [" + r.fp.srv.URL + "]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.decls.consumers["tools-mcp/fixed"] = c
	if res, cerr := r.useKey("service:pony-a", "fixed", r.fp.srv.URL); cerr != nil || res.Token != testKey {
		t.Fatalf("token %q err %v", res.Token, cerr)
	}
	if _, cerr := r.putKey("service:pony-a", "fixed:x", r.fp.srv.URL, testKey); cerr == nil || cerr.Code != CodeInvalidArg {
		t.Fatalf("an instance of a fixed provider: %v", cerr)
	}
}

func TestPastedKeyDeclarations(t *testing.T) {
	bad := map[string]string{
		"oauth fields":           "kind: api_key\ninstances: true\nclient_id: x\nconsumers: {a: [b.c]}\n",
		"a token destination":    "kind: api_key\ninstances: true\ndestinations: {token: https://a.example/t}\nconsumers: {a: [b.c]}\n",
		"instances and origins":  "kind: api_key\ninstances: true\ndestinations: {api: [https://a.example]}\nconsumers: {a: [b.c]}\n",
		"neither":                "kind: api_key\nconsumers: {a: [b.c]}\n",
		"instances on a sign-in": "kind: oauth2\ninstances: true\ndestinations: {authorization: https://a.example/a, token: https://a.example/t, api: [https://a.example]}\nclient_id: x\nclient_secret: secret:Y\nconsumers: {a: [b.c]}\n",
	}
	for name, y := range bad {
		if _, err := ParseProvider([]byte(y)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := ParseConsumer([]byte("provider: github\n")); err == nil {
		t.Error("a sign-in consumer with no origins parsed")
	}
}

func TestTakePastedKeyHandsItOverOnce(t *testing.T) {
	ctx := WithPastedKey(context.Background(), []byte(testKey))
	if got := TakePastedKey(ctx); string(got) != testKey {
		t.Fatalf("first take: %q", got)
	}
	if got := TakePastedKey(ctx); got != nil {
		t.Fatalf("second take: %q", got)
	}
	if got := TakePastedKey(context.Background()); got != nil {
		t.Fatalf("no key: %q", got)
	}
}
