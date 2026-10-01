package ocitoken

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jws"
)

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// testPair makes a P-256 key and a self-signed certificate for it, valid
// around testNow, the way `openssl req -x509` would: no CA flag, no EKU.
func testPair(t *testing.T, curve elliptic.Curve, notBefore, notAfter time.Time) (keyPEM, certPEM []byte, key *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pkcs8PEM(t, key), certFor(t, key, notBefore, notAfter), key
}

func pkcs8PEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func certFor(t *testing.T, key *ecdsa.PrivateKey, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "registry token signer"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func testSigner(t *testing.T) (*Signer, []byte) {
	t.Helper()
	keyPEM, certPEM, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(24*time.Hour))
	s, err := Load(Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "https://admin.test", Service: "registry.test"}, testNow)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s, certPEM
}

var pushAccess = []Access{{Type: "repository", Name: "acme/tool", Actions: []string{"pull", "push"}}}

// distributionVerify re-implements what Distribution 2.8.3's
// registry/auth/token does to a token, in the order it does it: the key
// comes from the x5c header (standard base64 DER, x509-verified against
// the root bundle), the signature is raw r‖s, `aud` is a JSON STRING, and
// exp/nbf carry sixty seconds of leeway. It returns the access granted.
func distributionVerify(raw string, rootPEM []byte, issuer, service string, now time.Time) ([]Access, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("header: %v", err)
	}
	var hdr struct {
		Typ string   `json:"typ"`
		Alg string   `json:"alg"`
		X5c []string `json:"x5c"`
	}
	if err := json.Unmarshal(hdrJSON, &hdr); err != nil {
		return nil, fmt.Errorf("header: %v", err)
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("claims: %v", err)
	}
	// 2.8.3's ClaimSet: Audience is a string, so an array fails to decode.
	var cs struct {
		Issuer     string   `json:"iss"`
		Subject    string   `json:"sub"`
		Audience   string   `json:"aud"`
		Expiration int64    `json:"exp"`
		NotBefore  int64    `json:"nbf"`
		IssuedAt   int64    `json:"iat"`
		JWTID      string   `json:"jti"`
		Access     []Access `json:"access"`
	}
	if err := json.Unmarshal(claimsJSON, &cs); err != nil {
		return nil, fmt.Errorf("claims: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("signature: %v", err)
	}

	if cs.Issuer != issuer {
		return nil, errors.New("untrusted issuer")
	}
	if cs.Audience != service {
		return nil, errors.New("untrusted audience")
	}
	const leeway = 60 * time.Second
	if now.After(time.Unix(cs.Expiration, 0).Add(leeway)) {
		return nil, errors.New("token expired")
	}
	if now.Before(time.Unix(cs.NotBefore, 0).Add(-leeway)) {
		return nil, errors.New("token not yet valid")
	}
	if len(sig) == 0 {
		return nil, errors.New("no signature")
	}

	if len(hdr.X5c) == 0 {
		return nil, errors.New("no x5c")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("no roots")
	}
	var chain []*x509.Certificate
	for _, c := range hdr.X5c {
		der, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			return nil, fmt.Errorf("x5c: %v", err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("x5c: %v", err)
		}
		chain = append(chain, cert)
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		Intermediates: inter, Roots: roots, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return nil, fmt.Errorf("x5c chain: %v", err)
	}
	pub, ok := chain[0].PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("x5c leaf is not ECDSA")
	}
	if hdr.Alg != "ES256" || pub.Curve != elliptic.P256() {
		return nil, errors.New("alg does not match the key")
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("signature is %d bytes, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return nil, errors.New("invalid signature")
	}
	return cs.Access, nil
}

func TestMintIsAcceptedByDistribution(t *testing.T) {
	s, certPEM := testSigner(t)
	tok, err := s.Mint("acme/actor_1", "tnt_acme", pushAccess, testNow)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	got, err := distributionVerify(tok.Raw, certPEM, "https://admin.test", "registry.test", testNow)
	if err != nil {
		t.Fatalf("distribution refuses the token: %v", err)
	}
	if !reflect.DeepEqual(got, pushAccess) {
		t.Fatalf("access = %+v, want %+v", got, pushAccess)
	}
	if tok.Expires.Sub(tok.IssuedAt) != DefaultTTL {
		t.Fatalf("lifetime = %s, want %s", tok.Expires.Sub(tok.IssuedAt), DefaultTTL)
	}

	// The same token fails for another registry, another issuer, a bundle
	// that does not hold the certificate, and once it has expired.
	if _, err := distributionVerify(tok.Raw, certPEM, "https://admin.test", "other.test", testNow); err == nil {
		t.Error("accepted for another service")
	}
	if _, err := distributionVerify(tok.Raw, certPEM, "https://else.test", "registry.test", testNow); err == nil {
		t.Error("accepted from another issuer")
	}
	_, otherCert, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	if _, err := distributionVerify(tok.Raw, otherCert, "https://admin.test", "registry.test", testNow); err == nil {
		t.Error("accepted against a bundle without the certificate")
	}
	if _, err := distributionVerify(tok.Raw, certPEM, "https://admin.test", "registry.test", testNow.Add(DefaultTTL+2*time.Minute)); err == nil {
		t.Error("accepted after expiry")
	}
	// One flipped payload byte breaks the signature.
	parts := strings.Split(tok.Raw, ".")
	forged := parts[0] + "." + strings.Replace(parts[1], parts[1][:4], "AAAA", 1) + "." + parts[2]
	if _, err := distributionVerify(forged, certPEM, "https://admin.test", "registry.test", testNow); err == nil {
		t.Error("accepted a tampered payload")
	}
}

// The header and payload, field by field: what a registry on either major
// version reads.
func TestMintShape(t *testing.T) {
	keyPEM, certPEM, key := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(24*time.Hour))
	s, err := Load(Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "https://admin.test", Service: "registry.test", TTL: 2 * time.Minute}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Mint("acme/actor_1", "tnt_acme", pushAccess, testNow)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok.Raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}

	var hdr map[string]any
	mustDecode(t, parts[0], &hdr)
	if hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("header = %v", hdr)
	}
	if _, has := hdr["kid"]; has {
		t.Error("header carries a kid: Distribution 3.x reads it as a JWK thumbprint and would not find the key")
	}
	x5c, _ := hdr["x5c"].([]any)
	if len(x5c) != 1 {
		t.Fatalf("x5c = %v, want one certificate", hdr["x5c"])
	}
	block, _ := pem.Decode(certPEM)
	if x5c[0] != base64.StdEncoding.EncodeToString(block.Bytes) {
		t.Error("x5c[0] is not the standard base64 of the certificate's DER")
	}

	var cl map[string]any
	mustDecode(t, parts[1], &cl)
	if aud, ok := cl["aud"].(string); !ok || aud != "registry.test" {
		t.Fatalf("aud = %#v, want the JSON string \"registry.test\"", cl["aud"])
	}
	if cl["iss"] != "https://admin.test" || cl["sub"] != "acme/actor_1" || cl["tenant_id"] != "tnt_acme" {
		t.Fatalf("claims = %v", cl)
	}
	iat, exp, nbf := int64(cl["iat"].(float64)), int64(cl["exp"].(float64)), int64(cl["nbf"].(float64))
	if iat != testNow.Unix() || exp != testNow.Add(2*time.Minute).Unix() || nbf != testNow.Add(-30*time.Second).Unix() {
		t.Fatalf("iat=%d exp=%d nbf=%d", iat, exp, nbf)
	}
	if jti, _ := cl["jti"].(string); len(jti) != 32 || jti != tok.ID {
		t.Fatalf("jti = %v, token id = %q", cl["jti"], tok.ID)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %d bytes, err %v; want raw r‖s of 64", len(sig), err)
	}
	// An independent JOSE implementation agrees.
	if _, err := jws.Verify([]byte(tok.Raw), jws.WithKey(jwa.ES256, &key.PublicKey)); err != nil {
		t.Fatalf("jwx refuses the signature: %v", err)
	}

	// Two tokens for the same grant differ (jti), so an audit line names one.
	tok2, _ := s.Mint("acme/actor_1", "tnt_acme", pushAccess, testNow)
	if tok2.ID == tok.ID || tok2.Raw == tok.Raw {
		t.Error("two mints produced the same token")
	}
}

func mustDecode(t *testing.T, part string, into any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
}

func TestMintNeedsAccess(t *testing.T) {
	s, _ := testSigner(t)
	if _, err := s.Mint("acme/a", "tnt", nil, testNow); err == nil {
		t.Fatal("minted a token granting nothing")
	}
}

// A chassis that outlives its certificate stops minting: a token signed
// past NotAfter is one the registry would refuse.
func TestMintRefusesAfterCertificateExpiry(t *testing.T) {
	s, _ := testSigner(t) // valid until testNow + 24h
	if _, err := s.Mint("acme/a", "tnt", pushAccess, testNow.Add(23*time.Hour)); err != nil {
		t.Fatalf("within validity: %v", err)
	}
	_, err := s.Mint("acme/a", "tnt", pushAccess, testNow.Add(25*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want the certificate reported expired", err)
	}
}

func TestLoadTTLClamp(t *testing.T) {
	keyPEM, certPEM, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	for _, tc := range []struct{ in, want time.Duration }{
		{0, DefaultTTL}, {-1, DefaultTTL}, {5 * time.Second, MinTTL}, {2 * time.Hour, MaxTTL}, {7 * time.Minute, 7 * time.Minute},
	} {
		s, err := Load(Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "i", Service: "s", TTL: tc.in}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if s.TTL() != tc.want {
			t.Errorf("TTL %s → %s, want %s", tc.in, s.TTL(), tc.want)
		}
	}
}

func TestLoadKeyForms(t *testing.T) {
	_, certPEM, key := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	// What `openssl ecparam -name prime256v1 -genkey` writes: a parameters
	// block, then the SEC1 key.
	withParams := append(
		pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07}}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})...)
	for name, keyPEM := range map[string][]byte{"sec1 with parameters": withParams, "pkcs8": pkcs8PEM(t, key)} {
		s, err := Load(Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		block, _ := pem.Decode(certPEM)
		sum := sha256.Sum256(block.Bytes)
		if s.Fingerprint != fmt.Sprintf("%x", sum[:]) {
			t.Errorf("%s: fingerprint %s is not the certificate's SHA-256", name, s.Fingerprint)
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	keyPEM, certPEM, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	_, otherCert, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	p384Key, p384Cert, _ := testPair(t, elliptic.P384(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	edDER, _ := x509.MarshalPKCS8PrivateKey(edPriv)
	edPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER})

	for _, tc := range []struct {
		name string
		cfg  Config
		now  time.Time
		want string
	}{
		{"no issuer", Config{KeyPEM: keyPEM, CertPEM: certPEM, Service: "s"}, testNow, "empty issuer"},
		{"no service", Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "i"}, testNow, "empty service"},
		{"no key", Config{CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow, "no private key"},
		{"key is a cert", Config{KeyPEM: certPEM, CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow, "unexpected PEM block"},
		{"ed25519 key", Config{KeyPEM: edPEM, CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow, "not ECDSA"},
		{"p384 key", Config{KeyPEM: p384Key, CertPEM: p384Cert, Issuer: "i", Service: "s"}, testNow, "want P-256"},
		{"no cert", Config{KeyPEM: keyPEM, Issuer: "i", Service: "s"}, testNow, "no CERTIFICATE"},
		{"two certs", Config{KeyPEM: keyPEM, CertPEM: append(append([]byte{}, certPEM...), otherCert...), Issuer: "i", Service: "s"}, testNow, "more than one"},
		{"cert for another key", Config{KeyPEM: keyPEM, CertPEM: otherCert, Issuer: "i", Service: "s"}, testNow, "not for this key"},
		{"expired cert", Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow.Add(2 * time.Hour), "expired"},
		{"cert not yet valid", Config{KeyPEM: keyPEM, CertPEM: certPEM, Issuer: "i", Service: "s"}, testNow.Add(-2 * time.Hour), "not valid until"},
	} {
		_, err := Load(tc.cfg, tc.now)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestParseScope(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Access
	}{
		{"repository:acme/tool:pull,push", Access{"repository", "acme/tool", []string{"pull", "push"}}},
		{"repository:acme/tool:push,pull,push", Access{"repository", "acme/tool", []string{"pull", "push"}}},
		{"repository:acme/a/b:pull", Access{"repository", "acme/a/b", []string{"pull"}}},
		{"registry:catalog:*", Access{"registry", "catalog", []string{"*"}}},
		// A colon in the name stays in the name; whether that name is
		// acceptable is ValidateRepository's call.
		{"repository:host:5000/acme/tool:pull", Access{"repository", "host:5000/acme/tool", []string{"pull"}}},
	} {
		got, err := ParseScope(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	for _, in := range []string{"", "repository", "repository:acme/tool", ":acme/tool:pull", "repository::pull",
		"repository:acme/tool:", "repository:acme/tool:pull,", "repository:acme/tool:,push"} {
		if got, err := ParseScope(in); err == nil {
			t.Errorf("%q parsed as %+v, want an error", in, got)
		}
	}
}

func TestValidateRepository(t *testing.T) {
	for _, ok := range []string{"acme/tool", "acme/a/b", "a_b/tool", "a__b/x.y-z", "team-1/pkg", "a--b/c", "0/1",
		"acme/blobs", "acme/uploads/blobs", "acme/blobs/x/uploads", "blobs/uploadsx", "acme/xblobs/uploads"} {
		if err := ValidateRepository(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "tool", "Acme/tool", "acme/Tool", "acme//tool", "/acme/tool", "acme/tool/",
		"acme/-tool", "acme/tool-", "_sys/tool", "acme/to ol", "acme/tool:1", "acme/../x", "acme/a___b", "acme/*",
		"host:5000/acme/tool", "acme/" + strings.Repeat("a", 251),
		"acme/blobs/uploads", "acme/blobs/uploads/x", "acme/x/blobs/uploads/y", "blobs/uploads", "acme/tool\n"} {
		if err := ValidateRepository(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidateRepository("acme/" + strings.Repeat("a", 250)); err != nil {
		t.Errorf("a 255-byte name is refused: %v", err)
	}
}

func TestNamespace(t *testing.T) {
	for in, want := range map[string][2]string{
		"acme/tool": {"acme", "tool"}, "acme/a/b": {"acme", "a/b"}, "tool": {"tool", ""},
	} {
		ns, rest := Namespace(in)
		if ns != want[0] || rest != want[1] {
			t.Errorf("Namespace(%q) = %q, %q; want %q, %q", in, ns, rest, want[0], want[1])
		}
	}
}

func TestLoadPEM(t *testing.T) {
	_, certPEM, _ := testPair(t, elliptic.P256(), testNow.Add(-time.Hour), testNow.Add(time.Hour))
	if got, err := LoadPEM(certPEM, ""); err != nil || string(got) != string(certPEM) {
		t.Fatalf("raw: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(certPEM)
	for _, in := range []string{b64, strings.TrimRight(b64, "="), "  " + b64 + "\n"} {
		got, err := LoadPEM([]byte("ignored"), in)
		if err != nil || string(got) != string(certPEM) {
			t.Fatalf("b64 %q…: %v", in[:8], err)
		}
	}
	if _, err := LoadPEM(nil, "not base64!"); err == nil {
		t.Error("accepted a value that is not base64")
	}
	if _, err := LoadPEM(nil, base64.StdEncoding.EncodeToString([]byte("just text"))); err == nil {
		t.Error("accepted base64 that is not PEM")
	}
}
