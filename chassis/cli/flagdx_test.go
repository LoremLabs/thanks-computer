package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestBindTargetFlags(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	tf := bindTargetFlags(fs)
	for _, name := range []string{"target", "addr", "user", "pass", "profile", "tenant"} {
		if fs.Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
	if err := fs.Parse([]string{"--addr", "x", "--tenant", "acme", "--profile", "p"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if tf.Addr != "x" || tf.Tenant != "acme" || tf.Profile != "p" {
		t.Errorf("tf = %+v", tf)
	}
}

// flag-after-positional regression for the converted commands: a flag placed
// after the <stack> positional must be honored (pre-pflag, stdlib flag dropped
// it). We confirm an --addr that trails the positional targets that address
// (the unreachable-chassis error echoes the host).
func TestStackVerbsHonorFlagsAfterPositional(t *testing.T) {
	t.Run("versions", func(t *testing.T) {
		t.Setenv("TXCO_HOME", t.TempDir())
		var o, e bytes.Buffer
		code := runVersions([]string{"api", "--addr", "http://order-check.invalid:9"}, &o, &e)
		if code != 1 || !strings.Contains(e.String(), "order-check.invalid") {
			t.Fatalf("code=%d stderr=%q", code, e.String())
		}
	})
	t.Run("activate", func(t *testing.T) {
		t.Setenv("TXCO_HOME", t.TempDir())
		var o, e bytes.Buffer
		code := runActivate([]string{"api", "--addr", "http://order-check.invalid:9"}, &o, &e)
		if code != 1 || !strings.Contains(e.String(), "order-check.invalid") {
			t.Fatalf("code=%d stderr=%q", code, e.String())
		}
	})
	t.Run("trace", func(t *testing.T) {
		t.Setenv("TXCO_HOME", t.TempDir())
		var o, e bytes.Buffer
		code := runTrace([]string{"some-rid", "--addr", "http://order-check.invalid:9"}, &o, &e)
		if code == 0 || !strings.Contains(e.String(), "order-check.invalid") {
			t.Fatalf("code=%d stderr=%q", code, e.String())
		}
	})
}

// TestRunPullRootDiscovery: pull given a SUBDIR of the workspace resolves up to
// the OPS/ root (like apply/push), writing files at the root rather than the
// subdir.
func TestRunPullRootDiscovery(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := t.TempDir()
	sub := filepath.Join(root, "OPS", "api", "0")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/versions/3"):
			_, _ = w.Write([]byte(`{"version_number":3,"manifest_hash":"h","files":[{"path":"0/root.txcl","content":"EMIT .ok = \"y\""}]}`))
		case strings.HasSuffix(r.URL.Path, "/stacks/api"):
			_, _ = w.Write([]byte(`{"name":"api","active_version":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var o, e bytes.Buffer
	// Pass the SUBDIR as <dir>; root discovery must resolve up to `root`.
	code := runPull([]string{"api", sub, "--addr", srv.URL, "--force"}, &o, &e)
	if code != 0 {
		t.Fatalf("code=%d; stderr=%q", code, e.String())
	}
	if _, err := os.Stat(filepath.Join(root, "OPS", "api", "0", "root.txcl")); err != nil {
		t.Fatalf("pull didn't resolve to the workspace root: %v\nstdout=%s", err, o.String())
	}
}
