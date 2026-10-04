package tenants

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/pem"
	"errors"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// DKIMSelector is the fixed selector for chassis-issued DKIM keys. The key
// material differs per zone (per domain); the selector name is shared. The
// DNS head publishes the public key at <DKIMSelector>._domainkey.<origin>.
const DKIMSelector = "txco"

// GenerateDKIM creates an RSA-2048 DKIM keypair: the private key as PKCS#1
// PEM (the signer parses this) and the public key as base64-encoded PKIX DER
// (the `p=` tag the DNS head publishes). Called once per zone on the control
// plane (CreateZoneTx); the result fleet-syncs on the zone row.
func GenerateDKIM() (privPEM, pubB64 string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	return privPEM, base64.StdEncoding.EncodeToString(der), nil
}

// DKIMSignerForDomain returns the signing material for `domain`: the DKIM d=
// (SDID), selector, and private-key PEM. ok=false when nothing covers it.
// Resolution order:
//  1. An EXACT chassis-minted structured host (`tenant_hostnames` with a
//     per-host key) → d=<host> — per-host reputation isolation.
//  2. Else the most-specific delegated zone (`dns_zones` longest-match) →
//     d=<zone origin>.
//
// Used by the sendmail op.
func DKIMSignerForDomain(ctx context.Context, db *sql.DB, domain string, d registry.Dialect) (sdid, selector, privPEM string, ok bool, err error) {
	canon, cok := CanonicalizeHost(domain)
	if !cok {
		return "", "", "", false, nil
	}
	d = orSQLite(d)
	// 1. Per-host key on the exact structured host → sign as the host itself.
	err = db.QueryRowContext(ctx,
		d.Rebind(`SELECT dkim_selector, dkim_private_pem FROM tenant_hostnames
		  WHERE hostname = ? AND revoked_at IS NULL AND dkim_private_pem != ''
		  LIMIT 1`), canon).Scan(&selector, &privPEM)
	switch {
	case err == nil:
		return canon, selector, privPEM, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", "", false, err
	}
	// 2. Delegated-zone key (apex or subdomain; most-specific wins).
	err = db.QueryRowContext(ctx,
		d.Rebind(`SELECT origin, dkim_selector, dkim_private_pem FROM dns_zones
		  WHERE revoked_at IS NULL AND verified_at IS NOT NULL AND dkim_private_pem != ''
		    AND (origin = ? OR ? LIKE '%.' || origin)
		  ORDER BY length(origin) DESC LIMIT 1`),
		canon, canon).Scan(&sdid, &selector, &privPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return sdid, selector, privPEM, true, nil
}

// DKIMPublicKeyForTenant returns the public half of the DKIM key the chassis
// holds for `domain` (base64 PKIX DER, the DNS `p=` value) and its selector
// — but only when the domain is tenant `slug`'s OWN signing domain: an exact
// chassis-minted host of that tenant with a per-host key, or a delegated
// zone of that tenant whose origin is the domain. Those are exactly the d=
// values DKIMSignerForDomain signs that tenant's mail as. ok=false when the
// tenant has no key for the domain; another tenant's domain is never ok.
//
// Used by the LMTP head to verify a message's signature against the key
// this chassis signed with — no DNS, no network (the head's own-DKIM fact).
func DKIMPublicKeyForTenant(ctx context.Context, db *sql.DB, slug, domain string, d registry.Dialect) (selector, pubB64 string, ok bool, err error) {
	canon, cok := CanonicalizeHost(domain)
	if !cok || slug == "" || db == nil {
		return "", "", false, nil
	}
	d = orSQLite(d)
	var privPEM string
	err = db.QueryRowContext(ctx,
		d.Rebind(`SELECT h.dkim_selector, h.dkim_public_b64, h.dkim_private_pem
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ? AND t.slug = ?
		    AND h.revoked_at IS NULL AND t.revoked_at IS NULL
		    AND h.dkim_private_pem != ''
		  LIMIT 1`), canon, slug).Scan(&selector, &pubB64, &privPEM)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRowContext(ctx,
			d.Rebind(`SELECT z.dkim_selector, z.dkim_public_b64, z.dkim_private_pem
			   FROM dns_zones z
			   JOIN tenants t ON t.tenant_id = z.tenant_id
			  WHERE z.origin = ? AND t.slug = ?
			    AND z.revoked_at IS NULL AND z.verified_at IS NOT NULL AND t.revoked_at IS NULL
			    AND z.dkim_private_pem != ''
			  LIMIT 1`), canon, slug).Scan(&selector, &pubB64, &privPEM)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if pubB64 == "" {
		// An older row with no stored public half: it is the private key's.
		if pubB64, err = dkimPublicFromPrivate(privPEM); err != nil {
			return "", "", false, err
		}
	}
	return selector, pubB64, true, nil
}

func dkimPublicFromPrivate(privPEM string) (string, error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return "", errors.New("no PEM block in DKIM private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}
