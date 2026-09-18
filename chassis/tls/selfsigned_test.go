package tls

import (
	"os"
	"path/filepath"
	"testing"
)

// The dev certificate must cover the dev-local names a developer will
// type into a client (mail client, IRC client), plus loopback.
func TestSelfSignedDevHosts(t *testing.T) {
	cfg, err := SelfSignedTLS(append(DevSelfSignedHosts, "imap.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	leaf := cfg.Certificates[0].Leaf
	for _, host := range []string{"localhost", "pony.local.thanks.computer", "a.localhost", "imap.example.test"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Errorf("%s not covered: %v", host, err)
		}
	}
	if err := leaf.VerifyHostname("deep.pony.local.thanks.computer"); err == nil {
		t.Error("a two-label wildcard match must fail (that is the manual-server-entry note in the design doc)")
	}
	if leaf.VerifyHostname("127.0.0.1") != nil {
		t.Error("loopback IP not covered")
	}
}

func TestLoadOrMintSelfSignedReuses(t *testing.T) {
	dir := t.TempDir()
	crt, key := filepath.Join(dir, "dev.crt"), filepath.Join(dir, "dev.key")
	hosts := []string{"localhost", "127.0.0.1", "*.local.thanks.computer"}
	c1, minted, err := LoadOrMintSelfSigned(crt, key, hosts)
	if err != nil || !minted {
		t.Fatalf("first: minted=%v err=%v", minted, err)
	}
	c2, minted, err := LoadOrMintSelfSigned(crt, key, hosts)
	if err != nil || minted {
		t.Fatalf("second: minted=%v err=%v", minted, err)
	}
	if string(c1.Certificates[0].Certificate[0]) != string(c2.Certificates[0].Certificate[0]) {
		t.Error("second boot must serve the same certificate")
	}
	// A different host set mints a new one.
	c3, minted, _ := LoadOrMintSelfSigned(crt, key, append(hosts, "imap.example.test"))
	if !minted || string(c3.Certificates[0].Certificate[0]) == string(c1.Certificates[0].Certificate[0]) {
		t.Error("changed hosts must re-mint")
	}
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key perms = %v err=%v", st.Mode(), err)
	}
}
