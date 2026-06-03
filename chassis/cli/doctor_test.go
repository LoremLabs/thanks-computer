package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
	"github.com/loremlabs/thanks-computer/chassis/cli/update"
)

// findingFor returns the (unique-per-section) finding with the given label.
func findingFor(s section, label string) *finding {
	for i := range s.Findings {
		if s.Findings[i].Label == label {
			return &s.Findings[i]
		}
	}
	return nil
}

// writeProfileFixture writes a real OpenSSH ed25519 key + a meta file for
// `name` under the (test-scoped) $TXCO_HOME, so the signer actually loads —
// exercising the "signing ready" path rather than just the error branches.
func writeProfileFixture(t *testing.T, name, chassisURL string, keyMode os.FileMode) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	kp, err := auth.KeyPath(name)
	if err != nil {
		t.Fatalf("KeyPath: %v", err)
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(block), keyMode); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.Chmod(kp, keyMode); err != nil { // WriteFile is umask-subject
		t.Fatalf("chmod key: %v", err)
	}
	mp, err := auth.MetaPath(name)
	if err != nil {
		t.Fatalf("MetaPath: %v", err)
	}
	if err := auth.SaveMeta(mp, auth.Meta{
		ActorID: "actor_x", KeyID: "key_x", ChassisURL: chassisURL, KeySource: auth.SourceFile,
	}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
}

func TestDoctorSigningReady(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	writeProfileFixture(t, "local", "http://localhost:8081", 0o600)

	sec := signingSection("")
	if f := findingFor(sec, "signing"); f == nil || f.Status != statusOK {
		t.Fatalf("want signing=✓ ready, got %+v (section=%+v)", f, sec)
	}
	if f := findingFor(sec, "key_id"); f == nil || f.Value != "key_x" {
		t.Errorf("key_id finding = %+v, want value key_x", f)
	}
	if f := findingFor(sec, "backend"); f == nil || f.Value != "file" {
		t.Errorf("backend finding = %+v, want value file", f)
	}
	if f := findingFor(sec, "bound chassis"); f == nil || f.Value != "http://localhost:8081" {
		t.Errorf("bound chassis finding = %+v", f)
	}
}

func TestDoctorSigningKeyPerms(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	writeProfileFixture(t, "local", "", 0o644) // too-open key file

	sec := signingSection("")
	if f := findingFor(sec, "key permissions"); f == nil || f.Status != statusWarn {
		t.Fatalf("want key-permissions=⚠ for 0644 key, got %+v", f)
	}
}

func TestDoctorSigningStates(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	// No key configured at all → "not configured" warning.
	sec := signingSection("")
	if f := findingFor(sec, "signing"); f == nil || f.Status != statusWarn ||
		!strings.Contains(f.Value, "not configured") {
		t.Fatalf("want not-configured ⚠, got %+v", f)
	}

	// Logged out (active=none) → "disabled" warning, sent unsigned.
	if err := auth.WriteActiveProfile(auth.ActiveNone); err != nil {
		t.Fatalf("WriteActiveProfile: %v", err)
	}
	sec = signingSection("")
	if f := findingFor(sec, "signing"); f == nil || f.Status != statusWarn ||
		!strings.Contains(f.Value, "disabled") {
		t.Fatalf("want logged-out ⚠, got %+v", f)
	}
}

func TestClassifySignerError(t *testing.T) {
	cases := []struct {
		err       error
		wantLabel string
	}{
		{signer.ErrNoAgent, "ssh-agent"},
		{signer.ErrKeyNotInAgent, "ssh-agent"},
		{&signer.PassphraseMissingError{Path: "/k.ed25519"}, "key"},
		{errors.New("some other failure"), "signing"},
	}
	for _, c := range cases {
		f := classifySignerError(c.err)
		if f.Status != statusFail {
			t.Errorf("%v: status=%v, want fail", c.err, f.Status)
		}
		if f.Label != c.wantLabel {
			t.Errorf("%v: label=%q, want %q", c.err, f.Label, c.wantLabel)
		}
		if f.Hint == "" {
			t.Errorf("%v: expected a fix hint, got none", c.err)
		}
	}
}

func TestDoctorChassisSection(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"0.9.0","commit":"abcdef123456",` +
			`"client":{"latest":"0.9.0","minimum_supported":"0.9.0","critical":false}}`))
	})
	mux.HandleFunc("/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source":"signed","actor_id":"actor_x","capabilities":["admin:*:*"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sec, info, ok := chassisSection("", srv.URL)
	if !ok {
		t.Fatal("expected srvOK=true for a reachable /healthz")
	}
	if info.Version != "0.9.0" {
		t.Errorf("server info Version=%q, want 0.9.0", info.Version)
	}
	if f := findingFor(sec, "reachable"); f == nil || f.Status != statusOK {
		t.Errorf("reachable finding = %+v, want ✓", f)
	}
	if f := findingFor(sec, "identity"); f == nil || f.Status != statusOK ||
		!strings.Contains(f.Value, "signed") {
		t.Errorf("identity finding = %+v, want ✓ signed", f)
	}
}

func TestDoctorChassisUnreachable(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close() // now guaranteed connection-refused

	sec, _, ok := chassisSection("", dead)
	if ok {
		t.Error("expected srvOK=false for an unreachable chassis")
	}
	if f := findingFor(sec, "reachable"); f == nil || f.Status != statusFail {
		t.Errorf("reachable finding = %+v, want ✗", f)
	}
}

func TestDoctorUpdatesServerPolicy(t *testing.T) {
	old := Build.Version
	Build.Version = "0.1.0"
	t.Cleanup(func() { Build.Version = old })

	// CLI below the server's minimum + critical → ✗.
	crit := update.ServerInfo{
		Version: "0.9.0",
		Client:  &update.Policy{MinimumSupported: "0.9.0", Latest: "0.9.0", Critical: true},
	}
	sec := updatesSection(crit, true, true /* offline: skip GitHub */)
	if f := findingFor(sec, "server policy"); f == nil || f.Status != statusFail {
		t.Fatalf("want server-policy ✗ (critical, below min), got %+v", f)
	}
	if f := findingFor(sec, "release check"); f == nil || !strings.Contains(f.Value, "skipped") {
		t.Errorf("offline release-check finding = %+v, want skipped", f)
	}

	// CLI at/above minimum → ✓ in sync.
	ok := update.ServerInfo{Client: &update.Policy{MinimumSupported: "0.1.0"}}
	sec = updatesSection(ok, true, true)
	if f := findingFor(sec, "server policy"); f == nil || f.Status != statusOK {
		t.Fatalf("want server-policy ✓ (in sync), got %+v", f)
	}
}

func TestDoctorRenderTextNoANSI(t *testing.T) {
	secs := []section{{Title: "X", Findings: []finding{
		{Status: statusOK, Label: "a", Value: "1"},
		{Status: statusFail, Label: "b", Value: "2", Hint: "do z"},
	}}}
	var buf bytes.Buffer
	renderDoctorText(&buf, secs) // a bytes.Buffer is not a TTY → no ANSI
	out := buf.String()
	if out != stripANSI(out) {
		t.Errorf("ANSI escape leaked into non-TTY output: %q", out)
	}
	if !strings.Contains(out, "✓ a: 1") {
		t.Errorf("missing ok line in:\n%s", out)
	}
	if !strings.Contains(out, "✗ b: 2") || !strings.Contains(out, "→ do z") {
		t.Errorf("missing fail line + hint in:\n%s", out)
	}
	if !anyFail(secs) {
		t.Error("anyFail should report true when a ✗ is present")
	}
}

func TestDoctorRenderJSON(t *testing.T) {
	secs := []section{{Title: "X", Findings: []finding{
		{Status: statusWarn, Label: "a", Value: "1", Hint: "h"},
	}}}
	var buf bytes.Buffer
	if err := renderDoctorJSON(&buf, secs); err != nil {
		t.Fatalf("renderDoctorJSON: %v", err)
	}
	var got []struct {
		Title    string `json:"title"`
		Findings []struct {
			Status string `json:"status"`
			Label  string `json:"label"`
			Hint   string `json:"hint"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, buf.String())
	}
	if len(got) != 1 || got[0].Title != "X" || len(got[0].Findings) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	if got[0].Findings[0].Status != "warn" || got[0].Findings[0].Hint != "h" {
		t.Errorf("finding = %+v, want status=warn hint=h", got[0].Findings[0])
	}
}
