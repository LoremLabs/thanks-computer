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
func DKIMSignerForDomain(ctx context.Context, db *sql.DB, domain string) (sdid, selector, privPEM string, ok bool, err error) {
	canon, cok := CanonicalizeHost(domain)
	if !cok {
		return "", "", "", false, nil
	}
	// 1. Per-host key on the exact structured host → sign as the host itself.
	err = db.QueryRowContext(ctx,
		`SELECT dkim_selector, dkim_private_pem FROM tenant_hostnames
		  WHERE hostname = ? AND revoked_at IS NULL AND dkim_private_pem != ''
		  LIMIT 1`, canon).Scan(&selector, &privPEM)
	switch {
	case err == nil:
		return canon, selector, privPEM, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", "", "", false, err
	}
	// 2. Delegated-zone key (apex or subdomain; most-specific wins).
	err = db.QueryRowContext(ctx,
		`SELECT origin, dkim_selector, dkim_private_pem FROM dns_zones
		  WHERE revoked_at IS NULL AND dkim_private_pem != ''
		    AND (origin = ? OR ? LIKE '%.' || origin)
		  ORDER BY length(origin) DESC LIMIT 1`,
		canon, canon).Scan(&sdid, &selector, &privPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return sdid, selector, privPEM, true, nil
}
