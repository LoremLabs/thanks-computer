package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/clicmd"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

func TestCLIExec(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open"})

	clicmd.Register("echotest", func(_ context.Context, args []string) (clicmd.Result, error) {
		return clicmd.Result{Stdout: "args=" + strings.Join(args, ","), Exit: 0}, nil
	})

	call := func(req *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		c.handleCLIExec(rr, req)
		return rr
	}
	post := func(args []string) *http.Request {
		body := mustJSON(t, map[string]any{"args": args})
		return httptest.NewRequest(http.MethodPost, "/v1/cli", bytes.NewReader(body))
	}

	// Known command + super-admin → 200, runs with args[1:].
	rr := call(withSuperAdminTenantContext(post([]string{"echotest", "x", "y"}), "tnt_default", "default"))
	if rr.Code != http.StatusOK {
		t.Fatalf("known/super-admin status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp cliExecResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stdout != "args=x,y" || resp.Exit != 0 {
		t.Errorf("resp=%+v, want stdout=args=x,y exit=0", resp)
	}

	// Unknown command → 404 regardless of auth (clean CLI fall-through).
	rr = call(withSuperAdminTenantContext(post([]string{"definitelyunknown"}), "tnt_default", "default"))
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown command status=%d, want 404", rr.Code)
	}

	// Known command + NOT super-admin → 403.
	nonAdmin := &auth.Context{Source: "signed", ActorID: "actor_x", TenantID: "tnt_default", TenantSlug: "default"}
	req := post([]string{"echotest"}).WithContext(auth.WithContext(context.Background(), nonAdmin))
	if rr := call(req); rr.Code != http.StatusForbidden {
		t.Errorf("non-super-admin status=%d, want 403", rr.Code)
	}
}

// TestTenantCLIExec covers the tenant-scoped exec endpoint: membership-gated
// (not super-admin), tenant read off the auth context, unknown → 404.
func TestTenantCLIExec(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open"})

	clicmd.RegisterTenant("tenantecho", func(ctx context.Context, args []string) (clicmd.Result, error) {
		return clicmd.Result{Stdout: "tenant=" + auth.FromContext(ctx).TenantSlug + " args=" + strings.Join(args, ","), Exit: 0}, nil
	})

	call := func(ac *auth.Context, args []string) *httptest.ResponseRecorder {
		body := mustJSON(t, map[string]any{"args": args})
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/acme/cli", bytes.NewReader(body)).
			WithContext(auth.WithContext(context.Background(), ac))
		rr := httptest.NewRecorder()
		c.handleTenantCLIExec(rr, req)
		return rr
	}

	// Member (signed, non-super-admin, membership caps for this tenant) → 200,
	// runs with the resolved tenant on ctx.
	member := &auth.Context{Source: "signed", ActorID: "actor_m", TenantSlug: "acme", TenantID: "tnt_acme", Capabilities: []string{"ops:acme:read"}}
	rr := call(member, []string{"tenantecho", "buy", "1"})
	if rr.Code != http.StatusOK {
		t.Fatalf("member status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp cliExecResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Stdout != "tenant=acme args=buy,1" {
		t.Errorf("resp=%+v, want tenant=acme args=buy,1", resp)
	}

	// Super-admin with no membership caps → 200 (operator override).
	admin := &auth.Context{Source: "signed", SuperAdmin: true, TenantSlug: "acme", TenantID: "tnt_acme"}
	if rr := call(admin, []string{"tenantecho"}); rr.Code != http.StatusOK {
		t.Errorf("super-admin status=%d, want 200", rr.Code)
	}

	// Non-member (signed, no caps for this tenant) → 403.
	nonMember := &auth.Context{Source: "signed", ActorID: "actor_x", TenantSlug: "acme", TenantID: "tnt_acme"}
	if rr := call(nonMember, []string{"tenantecho"}); rr.Code != http.StatusForbidden {
		t.Errorf("non-member status=%d, want 403", rr.Code)
	}

	// Unknown command → 404 regardless of auth (clean CLI fall-through).
	if rr := call(member, []string{"definitelyunknown"}); rr.Code != http.StatusNotFound {
		t.Errorf("unknown command status=%d, want 404", rr.Code)
	}
}
