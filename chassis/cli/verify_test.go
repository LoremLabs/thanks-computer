package cli

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
)

func TestLoadTrustedKeysFromConfig(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := priv.Public().(ed25519.PublicKey)
	sshPub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimRight(string(ssh.MarshalAuthorizedKey(sshPub)), "\n")

	dir := t.TempDir()
	yaml := "trust:\n  keys:\n    - name: ci\n      pubkey: \"" + line + "\"\n      registry: registry.thanks.computer\n"
	if err := os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	keys, err := loadTrustedKeys(dir, nil, &errb)
	if err != nil {
		t.Fatalf("loadTrustedKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 trusted key, got %d", len(keys))
	}
	if keys[0].KeyID != sign.KeyIDForPub(pub) || keys[0].Registry != "registry.thanks.computer" {
		t.Errorf("unexpected key: %+v", keys[0])
	}

	// A key may be narrowed to namespaces of its registry, and the same key
	// may be listed again with another scope: each entry stands.
	yaml = "trust:\n  keys:\n" +
		"    - name: onepony\n      pubkey: \"" + line + "\"\n      registry: registry.thanks.computer\n      namespaces: [onepony]\n" +
		"    - name: onepony-ghcr\n      pubkey: \"" + line + "\"\n      registry: ghcr.io\n" +
		"    - name: onepony\n      pubkey: \"" + line + "\"\n      registry: registry.thanks.computer\n      namespaces: [onepony]\n"
	if err := os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err = loadTrustedKeys(dir, nil, &errb)
	if err != nil || len(keys) != 2 {
		t.Fatalf("want 2 entries (the repeated one once), got %d %v", len(keys), err)
	}
	if len(keys[0].Namespaces) != 1 || keys[0].Namespaces[0] != "onepony" || keys[0].Registry != "registry.thanks.computer" {
		t.Errorf("namespace scope not loaded: %+v", keys[0])
	}
	if len(keys[1].Namespaces) != 0 || keys[1].Registry != "ghcr.io" {
		t.Errorf("second scope of the same key: %+v", keys[1])
	}

	// A bad --key flag is a hard error.
	if _, err := loadTrustedKeys(dir, []string{"not-a-key"}, &errb); err == nil {
		t.Error("bad --key flag should error")
	}
}

func TestEnforceSignaturePosture(t *testing.T) {
	trusted := sign.Verdict{Signed: true, Trusted: true, KeyID: "SHA256:abc"}
	untrusted := sign.Verdict{Signed: true, Reason: "signed by untrusted key"}
	unsigned := sign.Verdict{Reason: "no signature found"}

	var out, errb bytes.Buffer
	if !enforceSignaturePosture(trusted, true, &out, &errb) {
		t.Error("trusted+require should proceed")
	}
	out.Reset()
	errb.Reset()
	if enforceSignaturePosture(untrusted, true, &out, &errb) {
		t.Error("untrusted+require should NOT proceed")
	}
	out.Reset()
	errb.Reset()
	if !enforceSignaturePosture(unsigned, false, &out, &errb) {
		t.Error("unsigned without require should proceed (warn)")
	}
	if !strings.Contains(errb.String(), "warning") {
		t.Errorf("expected a warning, got %q", errb.String())
	}
}
