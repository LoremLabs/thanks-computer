package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/clientcmd"
)

// pingResp is the tiny JSON body the test endpoint returns.
type pingResp struct {
	OK bool `json:"ok"`
}

// TestClientCmdDispatch registers an overlay-style client verb, dispatches it
// through Dispatch with global connection flags, and asserts the handler ran,
// resolved a signed (here unsigned) tenant client, and reached the tenant-scoped
// endpoint via DoScoped.
func TestClientCmdDispatch(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir()) // isolate profile/key lookup

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ran := false
	clientcmd.Register("xtest", func(env clientcmd.Env, args []string) int {
		ran = true
		c, err := env.TenantClient()
		if err != nil {
			t.Errorf("TenantClient: %v", err)
			return 1
		}
		var out pingResp
		if err := c.DoScoped(context.Background(), http.MethodGet, "/test/ping", nil, &out); err != nil {
			t.Errorf("DoScoped: %v", err)
			return 1
		}
		if !out.OK {
			t.Errorf("want ok=true")
		}
		return 0
	})

	status, ok := Dispatch([]string{"txco", "xtest", "--addr", srv.URL, "--tenant", "acme"}, io.Discard, io.Discard)
	if !ok || status != 0 {
		t.Fatalf("Dispatch: status=%d ok=%v", status, ok)
	}
	if !ran {
		t.Fatal("handler did not run")
	}
	if want := "/v1/tenants/acme/test/ping"; gotPath != want {
		t.Fatalf("server saw path %q, want %q", gotPath, want)
	}
}

// TestClientCmdAdminDispatch checks the `admin <sub>` fallthrough reaches an
// overlay-registered admin handler.
func TestClientCmdAdminDispatch(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	ran := false
	clientcmd.RegisterAdmin("xsub", func(env clientcmd.Env, args []string) int {
		ran = true
		return 0
	})

	status, ok := Dispatch([]string{"txco", "admin", "xsub"}, io.Discard, io.Discard)
	if !ok || status != 0 {
		t.Fatalf("Dispatch admin: status=%d ok=%v", status, ok)
	}
	if !ran {
		t.Fatal("admin handler did not run")
	}
}
