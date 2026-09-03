package imap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// devSelfSignedHosts are the names a `txco dev --imap` certificate covers:
// every dev-local hostname pattern the chassis auto-verifies on bind (see
// tenants.IsDevLocalHostname) plus loopback. A mail client shows the
// certificate once and the developer trusts it; that is the whole point —
// desktop clients refuse to even attempt LOGIN over a plaintext port.
var devSelfSignedHosts = []string{
	"localhost", "127.0.0.1", "::1",
	"*.localhost", "*.local", "*.local.thanks.computer",
}

// SelfSignedTLS builds a self-signed ECDSA certificate for the given hosts
// (DNS names, wildcards, or IPs) valid for a year, and a TLS config serving
// it. Development only — the bundled cert manager or cert files are the
// real paths (§25.3). See LoadOrMintSelfSigned for the on-disk variant a
// developer can trust once in the OS keychain.
func SelfSignedTLS(hosts []string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("imap: self-signed key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("imap: serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "txco imap (self-signed, dev)", Organization: []string{"thanks.computer dev"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// Its own root: a verifier that is told to trust this certificate
		// (a developer's "Always Trust", the test's cert pool) needs it to
		// be a CA-capable self-signed leaf.
		IsCA: true,
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, strings.TrimSuffix(strings.ToLower(h), "."))
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("imap: self-signed cert: %w", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// LoadOrMintSelfSigned reuses the certificate at certPath/keyPath when it
// is still valid for 30 days and covers exactly these hosts; otherwise it
// mints a new one and writes both files (key 0600). A stable file is what
// lets a developer trust the certificate ONCE in the OS keychain instead
// of clicking through a warning on every chassis restart.
func LoadOrMintSelfSigned(certPath, keyPath string, hosts []string) (*tls.Config, bool, error) {
	want := hostSet(hosts)
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, perr := x509.ParseCertificate(cert.Certificate[0]); perr == nil &&
			time.Now().Add(30*24*time.Hour).Before(leaf.NotAfter) && hostSetOf(leaf) == want {
			cert.Leaf = leaf
			return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, false, nil
		}
	}
	cfg, err := SelfSignedTLS(hosts)
	if err != nil {
		return nil, false, err
	}
	c := cfg.Certificates[0]
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("imap: cert dir: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(c.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return nil, false, fmt.Errorf("imap: marshal key: %w", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, false, fmt.Errorf("imap: write key: %w", err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o644); err != nil {
		return nil, false, fmt.Errorf("imap: write cert: %w", err)
	}
	return cfg, true, nil
}

func hostSet(hosts []string) string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(h, ".")))
		if h != "" {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func hostSetOf(leaf *x509.Certificate) string {
	hosts := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		hosts = append(hosts, ip.String())
	}
	return hostSet(hosts)
}
