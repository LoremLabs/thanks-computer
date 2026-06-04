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
