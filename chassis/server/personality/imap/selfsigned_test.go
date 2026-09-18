package imap

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// With --imap-self-signed both STARTTLS and IMAPS must reach LOGIN on the
// certificate the head minted at Start. (Host coverage of the dev
// certificate itself is pinned in chassis/tls.)
func TestSelfSignedDevTLS(t *testing.T) {
	h := newHarness(t, config.Config{IMAPSelfSigned: true, IMAPTLSAddrs: []string{"127.0.0.1:0"}})
	h.account(t, "acme", "paris@example.com", "pw", "")
	// The head minted its own certificate at Start; trust THAT one.
	pool := x509.NewCertPool()
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
