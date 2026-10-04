package lmtp

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// The head's own-DKIM fact (`_txc.mail.auth.own.*`, read as `@mail.auth.own.*`).
//
// Every message txco://sendmail sends is signed with a key this chassis
// holds (chassis/mail dkimSign): a tenant's exact structured host, or its
// delegated zone. When such a message comes BACK to the same tenant — one
// address of the tenant's writing another — the edge often says nothing
// about it (a filter skips mail from its own network), so there is no
// Authentication-Results to read. The chassis that signed it can check the
// signature itself, against the key it signed with: no DNS, no network.
//
//	pass  a signature by one of the DELIVERY TENANT'S own domains verifies
//	      (`d` is that domain): the tenant's chassis sent these headers and
//	      this body as that domain, and they are unchanged
//	fail  a signature names one of the tenant's own domains and does not
//	      verify (altered, forged, a selector the tenant has no key for)
//	none  no signature by a domain of the tenant's
//
// A signature by any other domain is not this fact's business (that is the
// edge's verdict, `@mail.auth.dkim`), and another tenant's key never makes
// a pass: the key lookup is the delivery tenant's alone.
const (
	ownDKIMPass = "pass"
	ownDKIMFail = "fail"
	ownDKIMNone = "none"

	// More signatures than this on one message are not looked at: each one
	// by an own domain costs an RSA verify.
	ownDKIMMaxSignatures = 8
)

// errNotOwnDomain answers the verifier's key query for a domain that is not
// the delivery tenant's: the signature is left unverified, with no lookup
// anywhere.
var errNotOwnDomain = errors.New("lmtp: not a signing domain of this tenant")

// ownDKIM verifies the message's DKIM signatures against the delivery
// tenant's own keys. It returns pass | fail | none and, for a pass, the
// signing domain.
func ownDKIM(ctx context.Context, db *sql.DB, dia registry.Dialect, tenant string, msg []byte) (result, domain string) {
	if db == nil || tenant == "" || len(msg) == 0 {
		return ownDKIMNone, ""
	}
	own := map[string]bool{} // d= values that are this tenant's signing domains
	lookup := func(name string) ([]string, error) {
		// name is "<selector>._domainkey.<d>".
		selector, d, ok := strings.Cut(strings.ToLower(strings.TrimSuffix(name, ".")), "._domainkey.")
		if !ok {
			return nil, errNotOwnDomain
		}
		wantSelector, pubB64, found, err := tenants.DKIMPublicKeyForTenant(ctx, db, tenant, d, dia)
		if err != nil || !found {
			return nil, errNotOwnDomain
		}
		own[d] = true
		if selector != strings.ToLower(wantSelector) {
			return nil, errNotOwnDomain // the tenant's domain, a key it does not have
		}
		return []string{"v=DKIM1; k=rsa; p=" + pubB64}, nil
	}
	verifs, _ := dkim.VerifyWithOptions(bytes.NewReader(msg), &dkim.VerifyOptions{
		LookupTXT:        lookup,
		MaxVerifications: ownDKIMMaxSignatures,
	})
	result = ownDKIMNone
	for _, v := range verifs {
		if v == nil {
			continue
		}
		d := strings.ToLower(strings.TrimSuffix(v.Domain, "."))
		if !own[d] {
			continue
		}
		if v.Err == nil {
			return ownDKIMPass, d
		}
		result = ownDKIMFail
	}
	return result, ""
}
