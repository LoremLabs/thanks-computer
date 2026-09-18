package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// OAuthReauthOptions drives OAuthReauth: an ALREADY-ENROLLED profile signing
// back in after the admin UI signed it out.
type OAuthReauthOptions struct {
	EnrollEndpointURL string // the resolved /auth/oauth/enroll URL; the reauth URL is its sibling
	IDToken           string // bearer secret — never logged
	Profile           string // the enrolled chassis profile whose key signs back in
}

// OAuthReauthResult is what a caller needs to say who signed back in where.
type OAuthReauthResult struct {
	ActorID    string
	TenantSlug string
}

// ReauthMismatchError means the account the user just signed in with is not
// the one this profile's key was enrolled by. Nothing was changed.
type ReauthMismatchError struct {
	SignedInAs string
}

func (e *ReauthMismatchError) Error() string {
	return "signed in with a different account than this profile was enrolled with"
}

// OAuthReauth presents a fresh id_token together with the profile's EXISTING
// public key, which clears the chassis's "signed out — sign in again" marker
// on that key's actor. It never picks, generates or re-enrolls a key, and it
// never creates a tenant: signing back in with the wrong account is reported
// (ReauthMismatchError), not turned into a new cloud space.
func OAuthReauth(opts OAuthReauthOptions) (*OAuthReauthResult, error) {
	pub, err := profilePublicKeyB64(opts.Profile)
	if err != nil {
		return nil, err
	}
	cl := client.New(client.Target{Addr: opts.EnrollEndpointURL})
	ctx := context.Background()

	resp, err := cl.OAuthReauth(ctx, reauthEndpoint(opts.EnrollEndpointURL),
		client.OAuthReauthRequest{IDToken: opts.IDToken, PublicKey: pub})
	if err == nil {
		return &OAuthReauthResult{ActorID: resp.ActorID, TenantSlug: resp.TenantSlug}, nil
	}
	var he *client.HTTPError
	if !errors.As(err, &he) {
		return nil, err
	}
	switch {
	case he.StatusCode == http.StatusForbidden && (he.Code == "identity_not_enrolled" || he.Code == "identity_mismatch"):
		who, _ := he.Detail["signed_in_as"].(string)
		return nil, &ReauthMismatchError{SignedInAs: who}
	case he.StatusCode == http.StatusNotFound && he.Code != "key_not_enrolled":
		// A chassis that predates /auth/oauth/reauth. Its enroll endpoint is
		// what clears the marker there, and enrolling a key that is already
		// enrolled is idempotent — so present the SAME key, and treat "name
		// your new space" as what it means here: the wrong account.
		return reauthViaEnroll(ctx, cl, opts, pub)
	}
	return nil, err
}

func reauthViaEnroll(ctx context.Context, cl *client.Client, opts OAuthReauthOptions, pub string) (*OAuthReauthResult, error) {
	resp, err := cl.OAuthEnroll(ctx, opts.EnrollEndpointURL, client.OAuthEnrollRequest{
		IDToken:   opts.IDToken,
		PublicKey: pub,
		Profile:   opts.Profile,
	})
	if err == nil {
		return &OAuthReauthResult{ActorID: resp.ActorID, TenantSlug: resp.TenantSlug}, nil
	}
	var he *client.HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusConflict && slugConflictCodes[he.Code] {
		return nil, &ReauthMismatchError{}
	}
	return nil, err
}

// reauthEndpoint is the enroll endpoint's sibling: …/auth/oauth/reauth.
func reauthEndpoint(enrollURL string) string {
	if base, ok := strings.CutSuffix(strings.TrimRight(enrollURL, "/"), "/enroll"); ok {
		return base + "/reauth"
	}
	return strings.TrimRight(enrollURL, "/") + "/reauth"
}

// profilePublicKeyB64 is the enrolled profile's public key: from its meta
// when recorded there, else from the key itself.
func profilePublicKeyB64(profile string) (string, error) {
	metaPath, err := MetaPath(profile)
	if err != nil {
		return "", err
	}
	m, err := LoadMeta(metaPath)
	if err != nil {
		return "", fmt.Errorf("load profile %q: %w", profile, err)
	}
	if strings.TrimSpace(m.PublicKeyB64) != "" {
		return m.PublicKeyB64, nil
	}
	s, err := LoadSignerForMetaPath(metaPath)
	if err != nil {
		return "", fmt.Errorf("load profile %q key: %w", profile, err)
	}
	if s == nil {
		return "", fmt.Errorf("profile %q has no usable key", profile)
	}
	return PublicKeyB64(s.PublicKey()), nil
}

// ExistingKeyChoice says how to present an enrolled profile's OWN key to an
// enrollment, so re-enrolling a profile never silently binds it to a different
// key (and so a different actor) than the one it already has.
func ExistingKeyChoice(m *Meta, profile string) (sshKey string, sshAgent bool) {
	if m == nil {
		return "", false
	}
	if m.EffectiveKeySource() == SourceSSHAgent {
		return "", true
	}
	if p := strings.TrimSpace(m.KeyPath); p != "" {
		return p, false
	}
	if p, err := KeyPath(profile); err == nil {
		return p, false
	}
	return "", false
}
