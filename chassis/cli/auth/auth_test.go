package auth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withHome points TXCO_HOME at a fresh temp dir for the test's
// lifetime. Returned cleanup restores the prior env value.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TXCO_HOME", dir)
	// XDG fallback would intercept ahead of $HOME; clear it so the
	// resolver lands on TXCO_HOME deterministically.
	t.Setenv("XDG_CONFIG_HOME", "")
	return dir
}

func TestHomePathHonorsEnv(t *testing.T) {
	dir := withHome(t)
	got, err := HomePath()
	if err != nil {
		t.Fatalf("HomePath: %v", err)
	}
	if got != dir {
		t.Fatalf("HomePath = %q, want %q", got, dir)
	}
}

func TestKeyAndMetaPathLayout(t *testing.T) {
	dir := withHome(t)
	kp, err := KeyPath("local")
	if err != nil {
		t.Fatalf("KeyPath: %v", err)
	}
	want := filepath.Join(dir, "keys", "local.ed25519")
	if kp != want {
		t.Fatalf("KeyPath = %q, want %q", kp, want)
	}
	mp, err := MetaPath("local")
	if err != nil {
		t.Fatalf("MetaPath: %v", err)
	}
	if mp != want+".meta.json" {
		t.Fatalf("MetaPath = %q, want %q.meta.json", mp, want)
	}
	// The keys/ subdir is mode 0700.
	info, err := os.Stat(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatalf("stat keys dir: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("keys dir mode = %o, want 0700", perm)
	}
}

func TestKeysRoundtripPEM(t *testing.T) {
	withHome(t)
	path, err := KeyPath("rt")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if err := SavePrivateKey(path, priv); err != nil {
		t.Fatalf("save: %v", err)
	}
	// File should be 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 0600", perm)
	}

	loaded, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gotPub, ok := loaded.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("loaded public is not ed25519")
	}
	if !bytes.Equal(gotPub, pub) {
		t.Fatalf("public key mismatch after roundtrip")
	}
	// Second SavePrivateKey to same path must refuse.
	if err := SavePrivateKey(path, priv); err == nil {
		t.Fatalf("expected refusal to overwrite existing key")
	}
}

// TestSavePrivateKeyEmitsOpenSSHFormat — newly generated keys are
// written in the same PEM block (`OPENSSH PRIVATE KEY`) that
// `ssh-keygen -t ed25519` produces, so the file is interchangeable
// with `~/.ssh/id_ed25519` and standard ssh tooling can re-encrypt
// or move it without round-tripping through txco. Locks in the
// format-alignment goal of this change.
func TestSavePrivateKeyEmitsOpenSSHFormat(t *testing.T) {
	withHome(t)
	path, err := KeyPath("openssh-check")
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Probe just the PEM header — content can be opaque, but the
	// type tag is the human-readable signal that this is an SSH key.
	if !bytes.HasPrefix(raw, []byte("-----BEGIN OPENSSH PRIVATE KEY-----")) {
		t.Errorf("file should start with OPENSSH PRIVATE KEY block; first line:\n%s",
			bytes.SplitN(raw, []byte("\n"), 2)[0])
	}
}

// TestLoadPrivateKeyAcceptsLegacyPKCS8 — pre-upgrade developer keys
// (PKCS#8 PEM, `PRIVATE KEY` block) MUST keep loading after this
// change. The signer package's parser handles both formats so users
// don't need a migration step; this test pins that contract.
func TestLoadPrivateKeyAcceptsLegacyPKCS8(t *testing.T) {
	withHome(t)
	path, err := KeyPath("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	// Write a PKCS#8 PEM directly (the v1 format).
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatalf("load legacy PKCS#8: %v", err)
	}
	gotPub, ok := loaded.Public().(ed25519.PublicKey)
	if !ok || !bytes.Equal(gotPub, pub) {
		t.Errorf("legacy load returned wrong key")
	}
}

func TestPublicKeyB64Decodes(t *testing.T) {
	pub, _, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s := PublicKeyB64(pub)
	dec, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode b64: %v", err)
	}
	if !bytes.Equal(dec, pub) {
		t.Fatalf("decoded != original")
	}
}

func TestMetaRoundtrip(t *testing.T) {
	withHome(t)
	mp, err := MetaPath("rt")
	if err != nil {
		t.Fatal(err)
	}
	in := Meta{
		ActorID:    "actor_abc",
		KeyID:      "key_xyz",
		ChassisURL: "http://localhost:8081",
		Label:      "test",
		EnrolledAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveMeta(mp, in); err != nil {
		t.Fatalf("save meta: %v", err)
	}
	info, err := os.Stat(mp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("meta file mode = %o, want 0600", perm)
	}
	out, err := LoadMeta(mp)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if out.ActorID != in.ActorID || out.KeyID != in.KeyID || out.ChassisURL != in.ChassisURL || out.Label != in.Label {
		t.Fatalf("meta roundtrip mismatch: got %+v want %+v", out, in)
	}
	if !out.EnrolledAt.Equal(in.EnrolledAt) {
		t.Fatalf("enrolled_at mismatch: got %v want %v", out.EnrolledAt, in.EnrolledAt)
	}
}

// TestMetaRoundtripWithBackendFields — KeySource, PublicKeyB64,
// KeyPath survive save/load and decode to their canonical values.
// This is the contract sign-time dispatch depends on; if any of
// these fields silently drop on serialization, signed calls would
// pick the wrong backend.
func TestMetaRoundtripWithBackendFields(t *testing.T) {
	withHome(t)
	mp, _ := MetaPath("backend")
	in := Meta{
		ActorID:      "actor_a",
		KeyID:        "key_a",
		ChassisURL:   "http://localhost:8081",
		KeySource:    SourceSSHAgent,
		PublicKeyB64: "Zm9vYmFy", // anything decodable
		KeyPath:      "/home/me/.ssh/id_ed25519",
		EnrolledAt:   time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveMeta(mp, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadMeta(mp)
	if err != nil {
		t.Fatal(err)
	}
	if out.KeySource != SourceSSHAgent {
		t.Errorf("KeySource=%q, want %q", out.KeySource, SourceSSHAgent)
	}
	if out.PublicKeyB64 != in.PublicKeyB64 {
		t.Errorf("PublicKeyB64 roundtrip: got %q want %q", out.PublicKeyB64, in.PublicKeyB64)
	}
	if out.KeyPath != in.KeyPath {
		t.Errorf("KeyPath roundtrip: got %q want %q", out.KeyPath, in.KeyPath)
	}
}

// TestEffectiveKeySourceDefaultsToFile — meta files written before
// this change have no key_source field. They must continue to
// behave like file-backed entries; sign-time dispatch reads through
// EffectiveKeySource() so the empty-string case is never visible.
func TestEffectiveKeySourceDefaultsToFile(t *testing.T) {
	withHome(t)
	mp, _ := MetaPath("legacy")
	legacy := []byte(`{"actor_id":"actor_a","key_id":"key_a","chassis_url":"http://x","enrolled_at":"2026-05-12T00:00:00Z"}`)
	if err := os.MkdirAll(filepath.Dir(mp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mp, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := LoadMeta(mp)
	if err != nil {
		t.Fatal(err)
	}
	if out.KeySource != "" {
		t.Errorf("legacy meta should have empty KeySource on read; got %q", out.KeySource)
	}
	if got := out.EffectiveKeySource(); got != SourceFile {
		t.Errorf("EffectiveKeySource()=%q, want %q", got, SourceFile)
	}
}

func TestLoadMetaMissing(t *testing.T) {
	withHome(t)
	mp, err := MetaPath("nope")
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadMeta(mp)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

// TestBootstrapLocalHitsEnrollEndpoint stands up a fake /auth/dev/enroll,
// runs runBootstrapLocal end-to-end, and asserts both the wire shape
// and the on-disk artifacts.
func TestBootstrapLocalHitsEnrollEndpoint(t *testing.T) {
	withHome(t)
	// Pin the agent state so the test doesn't depend on whatever
	// ssh-agent the developer's shell happens to have running. We
	// want this test to exercise the fresh-keygen path explicitly.
	withoutAgent(t)
	withFakeHome(t)

	var capturedReq map[string]any
	var capturedSecret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/dev/enroll" {
			http.NotFound(w, r)
			return
		}
		capturedSecret = r.Header.Get("X-Txco-Enroll-Secret")
		if err := json.NewDecoder(r.Body).Decode(&capturedReq); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"actor_id":     "actor_test",
			"key_id":       "key_test",
			"capabilities": []string{"admin:all"},
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := runBootstrapLocal([]string{
		"--url", srv.URL,
		"--secret", "shh",
		"--label", "unit test",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runBootstrapLocal exit=%d stderr=%s", code, stderr.String())
	}

	if capturedSecret != "shh" {
		t.Fatalf("server saw secret=%q, want shh", capturedSecret)
	}
	if capturedReq["algorithm"] != "ed25519" {
		t.Fatalf("algorithm = %v, want ed25519", capturedReq["algorithm"])
	}
	pk, ok := capturedReq["public_key_b64"].(string)
	if !ok || pk == "" {
		t.Fatalf("missing/empty public_key_b64: %v", capturedReq["public_key_b64"])
	}
	dec, err := base64.StdEncoding.DecodeString(pk)
	if err != nil || len(dec) != ed25519.PublicKeySize {
		t.Fatalf("public_key_b64 doesn't decode to 32 bytes: len=%d err=%v", len(dec), err)
	}

	// Both key and meta should now exist on disk. The default key path
	// is the new ~/.ssh/id_ed25519-txco (withFakeHome points $HOME at
	// a tmp dir, so this is fully isolated).
	kp, err := defaultTxcoSSHKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kp); err != nil {
		t.Fatalf("expected key file at %q: %v", kp, err)
	}
	if _, err := os.Stat(kp + ".pub"); err != nil {
		t.Errorf("expected .pub sidecar at %q: %v", kp+".pub", err)
	}
	mp, _ := MetaPath("local")
	m, err := LoadMeta(mp)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}
	if m.ActorID != "actor_test" || m.KeyID != "key_test" {
		t.Fatalf("meta has wrong ids: %+v", m)
	}
	if m.ChassisURL != srv.URL {
		t.Fatalf("meta chassis_url = %q, want %q", m.ChassisURL, srv.URL)
	}
}

// TestBootstrapLocalRefusesOverwrite verifies the safety check.
// TestResolveSecretFromPipedStdin verifies the non-TTY branch: piped
// stdin yields a trimmed line. Covers the `echo <secret> | txco auth
// bootstrap-local` idiom that scripts will use.
func TestResolveSecretFromPipedStdin(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })

	go func() {
		_, _ = w.WriteString("  apple\n")
		_ = w.Close()
	}()

	got, err := resolveSecret("", new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolveSecret: %v", err)
	}
	if got != "apple" {
		t.Fatalf("got %q, want %q", got, "apple")
	}
}

// TestResolveSecretFlagWins — passing --secret never consults stdin.
func TestResolveSecretFlagWins(t *testing.T) {
	got, err := resolveSecret("flagval", new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolveSecret: %v", err)
	}
	if got != "flagval" {
		t.Fatalf("got %q, want flagval", got)
	}
}

// TestExplainEnrollErrAddsHint — 404 not_found should explain BOTH
// reasonable causes (an admin already enrolled, OR the chassis has
// no dev-enroll secret) and point at the right remediations. The
// invite/accept path is the more common follow-up after the first
// admin exists, so it must appear prominently.
func TestExplainEnrollErrAddsHint(t *testing.T) {
	in := errors.New(`404 Not Found: not_found (detail=map[hint:dev enrollment is not enabled on this chassis])`)
	out := explainEnrollErr(in)
	if out == nil {
		t.Fatalf("explainEnrollErr returned nil")
	}
	want := []string{
		"--auth-dev-enroll-secret", // the "no secret configured" branch
		"txco auth invite",         // the "already enrolled, onboard via invite" branch
		"txco auth accept",         // the invitee side of the invite flow
		"txco auth rotate-key",     // the "replace your current key" alternative
	}
	for _, w := range want {
		if !strings.Contains(out.Error(), w) {
			t.Errorf("hint lacks %q; got %v", w, out)
		}
	}
}

func TestBootstrapLocalRefusesOverwrite(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	withFakeHome(t)
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, []byte("pretend"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	// Force the fresh-keygen path so the collision check fires;
	// otherwise the auto-detect would take ssh-key / ssh-agent
	// branches that bypass the $TXCO_HOME key file.
	code := runBootstrapLocal([]string{"--url", "http://example", "--secret", "x", "--new-key"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("stderr lacks 'already exists': %s", stderr.String())
	}
}

// TestRunWhoamiSignsRequest spins up a mock server that verifies the
// outbound request carries Signature-Input + Signature + Content-Digest.
// We don't re-verify the signature itself here — chassis/auth/signature
// already covers that — we just confirm the wiring is in place.
func TestRunWhoamiSignsRequest(t *testing.T) {
	withHome(t)

	// Pre-create a key + meta so whoami has something to sign with.
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePrivateKey(kp, priv); err != nil {
		t.Fatal(err)
	}
	mp, _ := MetaPath("local")
	if err := SaveMeta(mp, Meta{
		ActorID:    "actor_a",
		KeyID:      "key_a",
		ChassisURL: "http://placeholder",
	}); err != nil {
		t.Fatal(err)
	}

	var sigInput, sig, digest string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sigInput = r.Header.Get("Signature-Input")
		sig = r.Header.Get("Signature")
		digest = r.Header.Get("Content-Digest")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"source":       "signed",
			"actor_id":     "actor_a",
			"key_id":       "key_a",
			"capabilities": []string{"admin:all"},
		})
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := runWhoami([]string{"--url", srv.URL}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runWhoami exit=%d stderr=%s", code, stderr.String())
	}
	if sigInput == "" || sig == "" || digest == "" {
		t.Fatalf("missing signature headers: input=%q sig=%q digest=%q", sigInput, sig, digest)
	}
	if !strings.Contains(sigInput, "key_a") {
		t.Fatalf("Signature-Input lacks key id: %q", sigInput)
	}
	if !strings.HasPrefix(digest, "sha-256=:") {
		t.Fatalf("Content-Digest unexpected: %q", digest)
	}
	if !strings.Contains(stdout.String(), "source: signed") {
		t.Fatalf("stdout lacks source: %s", stdout.String())
	}
}

// --- invitation accept tests ----------------------------------------------

// fakeConsumeServer stands in for /auth/invitations/consume. Records
// the request body so the test can assert correctness, and replies
// with synthetic ids.
func fakeConsumeServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/invitations/consume" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"actor_id":     "actor_invitee",
			"key_id":       "key_invitee",
			"capabilities": []string{"admin:all"},
		})
	}))
}

// TestAcceptHappy — passing --token on the command line redeems the
// invitation and writes the expected key + meta files.
func TestAcceptHappy(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	withFakeHome(t)
	var got map[string]any
	srv := fakeConsumeServer(t, &got)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	code := runAccept([]string{
		"--url", srv.URL,
		"--token", "apple-banana-cherry-date-elder-fig-grape-honey",
		"--label", "invitee",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runAccept exit=%d stderr=%s", code, stderr.String())
	}
	// Server saw the token + the new public key.
	if got["token"] != "apple-banana-cherry-date-elder-fig-grape-honey" {
		t.Errorf("server saw token=%v, want apple-…-honey", got["token"])
	}
	pk, _ := got["public_key_b64"].(string)
	if pk == "" {
		t.Errorf("missing public_key_b64 in request: %v", got)
	}
	// Files exist with the standard layout. Accept inherits the new
	// default key path (~/.ssh/id_ed25519-txco under the fake $HOME).
	kp, err := defaultTxcoSSHKeyPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kp); err != nil {
		t.Fatalf("expected key at %q: %v", kp, err)
	}
	mp, _ := MetaPath("local")
	m, err := LoadMeta(mp)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if m.ActorID != "actor_invitee" || m.KeyID != "key_invitee" {
		t.Errorf("meta: %+v", m)
	}
}

// TestPickFreeKeyNameDefaultFree — no existing key under the
// supplied name: returned silently. Migrated from the obsolete
// resolveAcceptKeyName tests; now exercises the canonical entry
// point for the rename UX (pickFreeKeyName).
func TestPickFreeKeyNameDefaultFree(t *testing.T) {
	withHome(t)
	name, kp, err := pickFreeKeyName("local", "", strings.NewReader(""), true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("pickFreeKeyName: %v", err)
	}
	if name != "local" {
		t.Errorf("name=%q, want local", name)
	}
	if !strings.HasSuffix(kp, "/keys/local.ed25519") {
		t.Errorf("keyPath=%q, want …/keys/local.ed25519", kp)
	}
}

// TestPickFreeKeyNameNonTTYConflict — default name taken without
// a TTY: error rather than hang on an unreachable prompt.
func TestPickFreeKeyNameNonTTYConflict(t *testing.T) {
	withHome(t)
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pickFreeKeyName("local", "", strings.NewReader(""), false, new(bytes.Buffer)); err == nil {
		t.Errorf("expected error on non-TTY conflict")
	}
}

// TestPickFreeKeyNamePromptsOnConflict — default name taken on a
// TTY: prompt loop suggests a label-derived name, accepts user
// input, returns the chosen name. Same UX ssh-keygen presents.
func TestPickFreeKeyNamePromptsOnConflict(t *testing.T) {
	withHome(t)
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("alice\n")
	var prompts bytes.Buffer
	name, got, err := pickFreeKeyName("local", "Matt", in, true, &prompts)
	if err != nil {
		t.Fatalf("pickFreeKeyName: %v", err)
	}
	if name != "alice" {
		t.Errorf("name=%q, want alice", name)
	}
	if !strings.HasSuffix(got, "/keys/alice.ed25519") {
		t.Errorf("keyPath=%q, want …/keys/alice.ed25519", got)
	}
	if !strings.Contains(prompts.String(), "already exists") {
		t.Errorf("prompt missing collision message: %q", prompts.String())
	}
	if !strings.Contains(prompts.String(), "matt") {
		t.Errorf("prompt should suggest label-derived name (matt); got %q", prompts.String())
	}
}

// TestPickFreeKeyNamePromptAcceptsDefault — pressing enter at the
// prompt accepts the suggested rename. Mirrors ssh-keygen.
func TestPickFreeKeyNamePromptAcceptsDefault(t *testing.T) {
	withHome(t)
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("\n") // user just hits enter
	name, _, err := pickFreeKeyName("local", "alice", in, true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("pickFreeKeyName: %v", err)
	}
	if name != "alice" {
		t.Errorf("name=%q, want alice (label-derived suggestion)", name)
	}
}

// TestAcceptFromStdin — when --token is omitted, the token is read
// from piped stdin (TTY-mode test would need a pty; we cover the
// non-TTY path here, which is what scripts use).
func TestAcceptFromStdin(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	withFakeHome(t)
	var got map[string]any
	srv := fakeConsumeServer(t, &got)
	defer srv.Close()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	go func() {
		_, _ = w.WriteString("piped-token-value-with-many-hyphenated-pieces\n")
		_ = w.Close()
	}()

	var stdout, stderr bytes.Buffer
	code := runAccept([]string{"--url", srv.URL, "--label", "invitee"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runAccept exit=%d stderr=%s", code, stderr.String())
	}
	if got["token"] != "piped-token-value-with-many-hyphenated-pieces" {
		t.Errorf("server saw token=%v, want the piped string", got["token"])
	}
}

// --- revoke-actor tests ---------------------------------------------------

// seedSignedProfile pre-creates a key + meta for the "local" profile
// under TXCO_HOME with the given actor id, so any command going through
// buildSignedTarget signs as that actor against the test server.
func seedSignedProfile(t *testing.T, actorID, chassisURL string) {
	t.Helper()
	kp, _ := KeyPath("local")
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		t.Fatal(err)
	}
	_, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePrivateKey(kp, priv); err != nil {
		t.Fatal(err)
	}
	mp, _ := MetaPath("local")
	if err := SaveMeta(mp, Meta{
		ActorID:    actorID,
		KeyID:      "key_" + actorID,
		ChassisURL: chassisURL,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRunRevokeActorHappyPath — super_admin revoking a different actor.
// whoami returns actor_admin, target is actor_target; CLI posts to the
// tenant-scoped revoke path and prints "revoked actor_target".
func TestRunRevokeActorHappyPath(t *testing.T) {
	withHome(t)

	var revokePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/auth/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"source":       "signed",
				"actor_id":     "actor_admin",
				"key_id":       "key_actor_admin",
				"capabilities": []string{"actor:*:revoke"},
			})
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			revokePath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revoked":  true,
				"actor_id": "actor_target",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	seedSignedProfile(t, "actor_admin", srv.URL)

	var stdout, stderr bytes.Buffer
	code := runRevokeActor([]string{"--url", srv.URL, "actor_target"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runRevokeActor exit=%d stderr=%s", code, stderr.String())
	}
	if want := "/v1/tenants/default/auth/actors/actor_target/revoke"; revokePath != want {
		t.Errorf("revoke path: got %q, want %q", revokePath, want)
	}
	if !strings.Contains(stdout.String(), "revoked actor_target") {
		t.Errorf("stdout: want `revoked actor_target`; got %q", stdout.String())
	}
}

// TestRunRevokeActorRefusesSelfRevoke — without --i-am-sure, the CLI's
// whoami-based guard must refuse and never POST. With --i-am-sure, it
// proceeds (the server's own 409 guard remains the load-bearing check;
// here we just verify the override flag takes effect).
func TestRunRevokeActorRefusesSelfRevoke(t *testing.T) {
	withHome(t)

	var sawRevoke bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/auth/whoami":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"source":       "signed",
				"actor_id":     "actor_admin",
				"key_id":       "key_actor_admin",
				"capabilities": []string{"actor:*:revoke"},
			})
		case strings.HasSuffix(r.URL.Path, "/revoke"):
			sawRevoke = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"revoked":  true,
				"actor_id": "actor_admin",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	seedSignedProfile(t, "actor_admin", srv.URL)

	// Bare self-revoke must refuse before POSTing.
	var stdout, stderr bytes.Buffer
	code := runRevokeActor([]string{"--url", srv.URL, "actor_admin"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("runRevokeActor self-revoke exit=%d (want 2) stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--i-am-sure") {
		t.Errorf("stderr should reference --i-am-sure; got %q", stderr.String())
	}
	if sawRevoke {
		t.Errorf("server saw a revoke POST despite client-side guard")
	}

	// With --i-am-sure, the CLI should skip the whoami check entirely
	// and proceed to POST (server's own 409 isn't simulated here — that
	// path is covered by the server-side test).
	stdout.Reset()
	stderr.Reset()
	code = runRevokeActor([]string{"--url", srv.URL, "--i-am-sure", "actor_admin"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runRevokeActor --i-am-sure exit=%d stderr=%s", code, stderr.String())
	}
	if !sawRevoke {
		t.Errorf("server did not see revoke POST despite --i-am-sure")
	}
}
