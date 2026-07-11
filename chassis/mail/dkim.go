package mail

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"

	"github.com/emersion/go-msgauth/dkim"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// dkimSign returns msg DKIM-signed with the per-domain key for the zone
// covering fromDomain (d=<zone origin>, s=<selector>), or the message
// unchanged when there's no key / on any error — signing is best-effort, an
// unsigned message still delivers (SPF covers it). Relaxed canonicalization
// survives the relay's header re-folding; the relay only adds unsigned
// headers (Received), leaving the signed set + body intact.
func (m *Mailer) dkimSign(ctx context.Context, fromDomain string, msg []byte) []byte {
	db, dia := m.readDB() // mirror read — runs per recipient, keys are mirrored
	origin, selector, privPEM, ok, err := tenants.DKIMSignerForDomain(ctx, db, fromDomain, dia)
	if err != nil {
		if m.log != nil {
			m.log.Warn("sendmail: DKIM key lookup failed", zap.String("domain", fromDomain), zap.Error(err))
		}
		return msg
	}
	if !ok {
		return msg // no zone key yet → unsigned
	}
	key, kerr := parseRSAPrivateKey(privPEM)
	if kerr != nil {
		if m.log != nil {
			m.log.Warn("sendmail: parse DKIM key", zap.String("domain", origin), zap.Error(kerr))
		}
		return msg
	}
	var buf bytes.Buffer
	if serr := dkim.Sign(&buf, bytes.NewReader(msg), &dkim.SignOptions{
		Domain:                 origin,
		Selector:               selector,
		Signer:                 key,
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	}); serr != nil {
		if m.log != nil {
			m.log.Warn("sendmail: DKIM sign", zap.String("domain", origin), zap.Error(serr))
		}
		return msg
	}
	return buf.Bytes()
}

func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block in DKIM private key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
