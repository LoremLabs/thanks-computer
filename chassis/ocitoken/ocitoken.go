// Package ocitoken mints the bearer tokens an OCI registry (CNCF
// Distribution, "token" auth) accepts: a short-lived JWT whose `access`
// claim names exact repositories and the actions allowed on each. The
// registry verifies a token by itself, against a certificate it was
// started with — it never calls back.
//
// The chassis admin plane is the token service: a signed admin request
// already says who is asking and for which tenant, so the handler
// (chassis/server/admin/registry_token.go) decides WHAT may be granted and
// this package only parses the request's scopes and signs the answer.
//
// Tokens are ES256 with the signer's certificate in the `x5c` header. A
// registry accepts an x5c leaf that is byte-identical to an entry of its
// rootcertbundle, so one self-signed certificate is the whole trust
// relationship, and the same token verifies on Distribution 2.8 and 3.x
// (which stopped matching the older `kid` form).
package ocitoken

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TTL bounds. A token is a bearer credential for a push: long enough for a
// large upload to start, short enough that a leaked one is worth little.
const (
	DefaultTTL = 300 * time.Second
	MinTTL     = 60 * time.Second
	MaxTTL     = 900 * time.Second

	// nbfSkew backdates a token so a registry whose clock runs slightly
	// behind the chassis does not refuse it as not yet valid.
	nbfSkew = 30 * time.Second
)

// Config is what a Signer is loaded from.
type Config struct {
	// KeyPEM is an ECDSA P-256 private key, PKCS#8 ("PRIVATE KEY") or SEC1
	// ("EC PRIVATE KEY"). An "EC PARAMETERS" block, as `openssl ecparam
	// -genkey` writes, is skipped.
	KeyPEM []byte
	// CertPEM is the key's certificate: exactly one CERTIFICATE block, the
	// same bytes the registry holds as its rootcertbundle.
	CertPEM []byte
	// Issuer is the `iss` claim (the registry's auth.token.issuer).
	Issuer string
	// Service is the `aud` claim (the registry's auth.token.service).
	Service string
	// TTL is a token's lifetime; zero is DefaultTTL, and it is clamped to
	// [MinTTL, MaxTTL].
	TTL time.Duration
}

// Signer mints tokens. Safe for concurrent use.
type Signer struct {
	key     *ecdsa.PrivateKey
	header  string // base64url of the fixed JOSE header
	issuer  string
	service string
	ttl     time.Duration

	// Fingerprint is the hex SHA-256 of the certificate's DER: what an
	// operator compares against the registry's bundle.
	Fingerprint string
	// NotAfter is when the certificate, and so every token, stops verifying.
	NotAfter time.Time
}

// Service is the registry this signer mints for (the `aud` claim).
func (s *Signer) Service() string { return s.service }

// Issuer is the `iss` claim.
func (s *Signer) Issuer() string { return s.issuer }

// TTL is the lifetime of a minted token.
func (s *Signer) TTL() time.Duration { return s.ttl }

// Load builds a Signer, refusing anything a registry would later refuse a
// token for: a key that is not P-256, a certificate that is not this
// key's, or one outside its validity at `now`.
func Load(cfg Config, now time.Time) (*Signer, error) {
	issuer, service := strings.TrimSpace(cfg.Issuer), strings.TrimSpace(cfg.Service)
	if issuer == "" {
		return nil, errors.New("ocitoken: empty issuer")
	}
	if service == "" {
		return nil, errors.New("ocitoken: empty service")
	}
	key, err := parseKey(cfg.KeyPEM)
	if err != nil {
		return nil, err
	}
	der, cert, err := parseCert(cfg.CertPEM)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("ocitoken: the certificate is not for this key")
	}
	if now.Before(cert.NotBefore) {
		return nil, fmt.Errorf("ocitoken: the certificate is not valid until %s", cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if !now.Before(cert.NotAfter) {
		return nil, fmt.Errorf("ocitoken: the certificate expired %s", cert.NotAfter.UTC().Format(time.RFC3339))
	}

	ttl := cfg.TTL
	switch {
	case ttl <= 0:
		ttl = DefaultTTL
	case ttl < MinTTL:
		ttl = MinTTL
	case ttl > MaxTTL:
		ttl = MaxTTL
	}

	// x5c is STANDARD base64 of the DER (RFC 7515 §4.1.6), unlike every
	// other part of a JWS.
	hdr, err := json.Marshal(struct {
		Alg string   `json:"alg"`
		Typ string   `json:"typ"`
		X5c []string `json:"x5c"`
	}{"ES256", "JWT", []string{base64.StdEncoding.EncodeToString(der)}})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return &Signer{
		key:         key,
		header:      base64.RawURLEncoding.EncodeToString(hdr),
		issuer:      issuer,
		service:     service,
		ttl:         ttl,
		Fingerprint: hex.EncodeToString(sum[:]),
		NotAfter:    cert.NotAfter,
	}, nil
}

func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("ocitoken: no private key in the PEM (want PRIVATE KEY or EC PRIVATE KEY)")
		}
		var key *ecdsa.PrivateKey
		switch block.Type {
		case "EC PARAMETERS":
			continue
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("ocitoken: parse EC private key: %v", err)
			}
			key = k
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("ocitoken: parse PKCS#8 private key: %v", err)
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, errors.New("ocitoken: the private key is not ECDSA (want P-256)")
			}
			key = ek
		default:
			return nil, fmt.Errorf("ocitoken: unexpected PEM block %q where a private key was expected", block.Type)
		}
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("ocitoken: the private key is on %s, want P-256", key.Curve.Params().Name)
		}
		return key, nil
	}
}

func parseCert(pemBytes []byte) ([]byte, *x509.Certificate, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("ocitoken: no CERTIFICATE block in the certificate PEM")
	}
	// One certificate, the registry's own bundle entry. A chain would need
	// its intermediates in x5c too; nothing here issues one.
	if next, _ := pem.Decode(rest); next != nil {
		return nil, nil, errors.New("ocitoken: the certificate PEM holds more than one block (want the one self-signed certificate)")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("ocitoken: parse certificate: %v", err)
	}
	return block.Bytes, cert, nil
}

// Access is one entry of a token's `access` claim: a resource and what may
// be done to it.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// ParseScope reads one scope as a registry challenge states it,
// `<type>:<name>:<action>[,<action>…]` (`repository:acme/tool:pull,push`).
// The type is what precedes the first colon and the actions what follows
// the last, so a name may itself hold one. Actions come back sorted and
// de-duplicated. It checks the shape only: which types, names and actions
// may be granted is the caller's decision.
func ParseScope(scope string) (Access, error) {
	first := strings.IndexByte(scope, ':')
	last := strings.LastIndexByte(scope, ':')
	if first <= 0 || last == first || last == len(scope)-1 {
		return Access{}, fmt.Errorf("scope %q is not <type>:<name>:<actions>", scope)
	}
	a := Access{Type: scope[:first], Name: scope[first+1 : last]}
	if a.Name == "" {
		return Access{}, fmt.Errorf("scope %q names nothing", scope)
	}
	seen := map[string]bool{}
	for _, act := range strings.Split(scope[last+1:], ",") {
		if act == "" {
			return Access{}, fmt.Errorf("scope %q has an empty action", scope)
		}
		if !seen[act] {
			seen[act] = true
			a.Actions = append(a.Actions, act)
		}
	}
	sort.Strings(a.Actions)
	return a, nil
}

// repositoryRe is the OCI distribution-spec repository name grammar.
var repositoryRe = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)

// MaxRepositoryLen is the longest repository name a registry takes.
const MaxRepositoryLen = 255

// ValidateRepository checks a repository name against the OCI grammar and
// requires a namespace: at least `<namespace>/<name>`.
func ValidateRepository(name string) error {
	if name == "" {
		return errors.New("empty repository name")
	}
	if len(name) > MaxRepositoryLen {
		return fmt.Errorf("repository name is longer than %d bytes", MaxRepositoryLen)
	}
	if !repositoryRe.MatchString(name) {
		return fmt.Errorf("repository name %q is not lowercase path components of letters, digits and single separators (. _ __ -)", name)
	}
	if !strings.Contains(name, "/") {
		return fmt.Errorf("repository name %q has no namespace (want <namespace>/<name>)", name)
	}
	// `…/blobs/uploads/…` is the registry API's upload-session route. A
	// repository whose own path holds that pair is legal OCI, but a proxy
	// that tells reads from writes by the route cannot tell its manifests
	// from an upload in flight.
	if strings.Contains("/"+name+"/", "/blobs/uploads/") {
		return fmt.Errorf("repository name %q holds the path blobs/uploads, which is the registry's upload route", name)
	}
	return nil
}

// Namespace splits a repository name at its FIRST slash: the namespace,
// and the package's name within it (which may itself be a path).
func Namespace(repository string) (namespace, rest string) {
	namespace, rest, _ = strings.Cut(repository, "/")
	return namespace, rest
}

// Token is a minted token and what a client is told about it.
type Token struct {
	Raw      string
	ID       string // the jti claim
	IssuedAt time.Time
	Expires  time.Time
}

// claims is marshalled by hand, in this order. `aud` must be a JSON
// string: Distribution 2.8 refuses the array form a JWT library writes.
type claims struct {
	Iss      string   `json:"iss"`
	Sub      string   `json:"sub"`
	Aud      string   `json:"aud"`
	Exp      int64    `json:"exp"`
	Nbf      int64    `json:"nbf"`
	Iat      int64    `json:"iat"`
	Jti      string   `json:"jti"`
	Access   []Access `json:"access"`
	TenantID string   `json:"tenant_id,omitempty"`
}

// Mint signs a token granting `access` to `sub`. tenantID is carried for
// the registry's log and a later audit; the registry does not read it.
func (s *Signer) Mint(sub, tenantID string, access []Access, now time.Time) (Token, error) {
	if len(access) == 0 {
		return Token{}, errors.New("ocitoken: no access to grant")
	}
	// Load checked the certificate once; a chassis can outlive it. A token
	// signed past NotAfter is one the registry refuses, so say so here
	// rather than hand out a token that cannot work.
	if !now.Before(s.NotAfter) {
		return Token{}, fmt.Errorf("ocitoken: the signing certificate expired %s", s.NotAfter.UTC().Format(time.RFC3339))
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Token{}, err
	}
	now = now.UTC().Truncate(time.Second)
	exp := now.Add(s.ttl)
	tok := Token{ID: hex.EncodeToString(id[:]), IssuedAt: now, Expires: exp}

	payload, err := json.Marshal(claims{
		Iss: s.issuer, Sub: sub, Aud: s.service,
		Exp: exp.Unix(), Nbf: now.Add(-nbfSkew).Unix(), Iat: now.Unix(),
		Jti: tok.ID, Access: access, TenantID: tenantID,
	})
	if err != nil {
		return Token{}, err
	}
	signing := s.header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return Token{}, fmt.Errorf("ocitoken: sign: %v", err)
	}
	// JWS ES256 is r‖s, each a fixed 32 bytes (RFC 7518 §3.4), not the
	// ASN.1 form crypto/ecdsa's SignASN1 writes.
	var sig [64]byte
	r.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	tok.Raw = signing + "." + base64.RawURLEncoding.EncodeToString(sig[:])
	return tok, nil
}

// LoadPEM returns the PEM for a caller holding either a file's bytes or its
// base64 (a secret store that keeps values, not files). The base64, when
// given, wins.
func LoadPEM(raw []byte, b64 string) ([]byte, error) {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return raw, nil
	}
	// Padding is optional: a secret UI may hand the value back without it.
	out, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
	if err != nil {
		return nil, fmt.Errorf("ocitoken: not base64: %v", err)
	}
	if !bytes.Contains(out, []byte("-----BEGIN ")) {
		return nil, errors.New("ocitoken: the base64 value does not decode to PEM")
	}
	return out, nil
}
