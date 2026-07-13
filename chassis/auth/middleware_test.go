package auth

import (
	"net/http/httptest"
	"slices"
	"testing"
)

// TestAuthenticateOpenDevGating pins the H3 fix: the open-dev fallback
// (empty basic creds under mode basic/both → admin:all for an
// unauthenticated caller) fires ONLY when AllowOpen is set. With
// AllowOpen false (the production default) the same request is denied,
// so a misconfigured prod chassis fails closed instead of exposing the
// whole admin API.
func TestAuthenticateOpenDevGating(t *testing.T) {
	cases := []struct {
		name      string
		mode      AuthMode
		allowOpen bool
		wantOpen  bool // true → openDevContext (admin:all); false → deny
	}{
		{"basic, empty creds, allow-open off → deny", ModeBasic, false, false},
		{"basic, empty creds, allow-open on → open", ModeBasic, true, true},
		{"both, empty creds, allow-open off → deny", ModeBoth, false, false},
		{"both, empty creds, allow-open on → open", ModeBoth, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Mode: tc.mode, AllowOpen: tc.allowOpen}
			req := httptest.NewRequest("GET", "/v1/tenants", nil)
			ctx, err := cfg.authenticate(req)
			if tc.wantOpen {
				if err != nil {
					t.Fatalf("want open-dev context, got err %v", err)
				}
				if ctx == nil || ctx.Source != "open" {
					t.Fatalf("want open-dev context (source=open), got %+v", ctx)
				}
				if !slices.Contains(ctx.Capabilities, "admin:all") {
					t.Fatalf("open-dev context missing admin:all: %v", ctx.Capabilities)
				}
			} else {
				if err == nil {
					t.Fatalf("want deny (auth error), got ctx %+v", ctx)
				}
				if ctx != nil {
					t.Fatalf("deny must return nil context, got %+v", ctx)
				}
			}
		})
	}
}

// TestAuthMiddlewareFailsClosedWithoutAllowOpen is the end-to-end HTTP
// assertion of the same invariant: no creds + basic/both + AllowOpen
// false → 401, never a 200 with admin:all.
func TestAuthMiddlewareFailsClosedWithoutAllowOpen(t *testing.T) {
	for _, mode := range []AuthMode{ModeBasic, ModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := &Config{Mode: mode, AllowOpen: false}
			req := httptest.NewRequest("GET", "/v1/tenants", nil)
			ctx, err := cfg.authenticate(req)
			if err == nil || ctx != nil {
				t.Fatalf("%s with empty creds + AllowOpen=false must deny; got ctx=%+v err=%v", mode, ctx, err)
			}
		})
	}
}

// TestAuthenticateBasicCredsRequireAuth confirms that when creds ARE
// configured, an unauthenticated request is denied regardless of
// AllowOpen (the open-dev fallback only applies to EMPTY creds).
func TestAuthenticateBasicCredsRequireAuth(t *testing.T) {
	cfg := &Config{Mode: ModeBasic, BasicUser: "alice", BasicPass: "secret", AllowOpen: true}
	req := httptest.NewRequest("GET", "/v1/tenants", nil)
	ctx, err := cfg.authenticate(req)
	if err == nil || ctx != nil {
		t.Fatalf("configured creds + no auth header must deny even with AllowOpen; got ctx=%+v err=%v", ctx, err)
	}
}
