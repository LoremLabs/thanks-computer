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

// DKIMSignerForDomain returns the signing material for the active zone
// covering `domain` (apex or subdomain, most-specific zone wins): the zone
// origin (the DKIM d= / SDID), the selector, and the private-key PEM.
// ok=false when no covering zone has a key yet. Used by the sendmail op.
func DKIMSignerForDomain(ctx context.Context, db *sql.DB, domain string) (origin, selector, privPEM string, ok bool, err error) {
	canon, cok := CanonicalizeHost(domain)
	if !cok {
		return "", "", "", false, nil
	}
	err = db.QueryRowContext(ctx,
		`SELECT origin, dkim_selector, dkim_private_pem FROM dns_zones
		  WHERE revoked_at IS NULL AND dkim_private_pem != ''
		    AND (origin = ? OR ? LIKE '%.' || origin)
		  ORDER BY length(origin) DESC LIMIT 1`,
		canon, canon).Scan(&origin, &selector, &privPEM)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	return origin, selector, privPEM, true, nil
}
