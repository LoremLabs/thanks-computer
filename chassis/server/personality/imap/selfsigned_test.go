package imap

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The dev certificate must cover the dev-local names a developer will type
// into a mail client, and both STARTTLS and IMAPS must reach LOGIN.
func TestSelfSignedDevTLS(t *testing.T) {
	cfg, err := SelfSignedTLS(append(devSelfSignedHosts, "imap.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	leaf := cfg.Certificates[0].Leaf
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
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

	h := newHarness(t, config.Config{IMAPSelfSigned: true, IMAPTLSAddrs: []string{"127.0.0.1:0"}})
	h.account(t, "acme", "paris@example.com", "pw", "")
	// The head minted its own certificate at Start; trust THAT one.
	pool = x509.NewCertPool()
	pool.AddCert(h.ctrl.tlsConfig.Certificates[0].Leaf)
	addrs := h.ctrl.boundAddrs()
	if len(addrs) != 2 {
		t.Fatalf("bound %v, want plaintext + IMAPS", addrs)
	}
	// STARTTLS on the plaintext port.
	c, err := imapclient.DialStartTLS(addrs[0], &imapclient.Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatalf("starttls: %v", err)
	}
	defer c.Close()
	if err := c.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatalf("login over starttls: %v", err)
	}
	// Implicit TLS on the IMAPS port, verifying against the minted cert.
	host, port, _ := net.SplitHostPort(addrs[1])
	c2, err := imapclient.DialTLS(net.JoinHostPort(host, port), &imapclient.Options{TLSConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}})
	if err != nil {
		t.Fatalf("imaps: %v", err)
	}
	defer c2.Close()
	if err := c2.Login("paris@example.com", "pw").Wait(); err != nil {
		t.Fatalf("login over imaps: %v", err)
	}
}

func TestLoadOrMintSelfSignedReuses(t *testing.T) {
	dir := t.TempDir()
	crt, key := SelfSignedPaths(dir + "/imap.db")
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
