package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// bindAccount is the block every account op shares — txco://imap/account,
// calendar/account, contacts/account and drive/account.
//
// An account holds no password. A login is a credential
// (txco://credential/create) that authenticates as a principal, and the
// account's username is what finds that principal: the op binds the
// username to `principal`. It does so on behalf of the calling stack, so
// the creator-stack rule covers accounts too — a stack can neither point
// another stack's principal at a username, nor change an account whose
// username another stack's principal holds.
//
// `principal` may be left out when the username is already bound: the
// binding supplies it, and the ownership check still runs.
//
// It returns the principal, or a code (to prefix with the op's family) and
// a message.
func bindAccount(ctx context.Context, ids *authn.Store, snap func() *sql.DB, tenantSlug, username string, meta []byte) (authn.Principal, string, string) {
	for _, k := range []string{"password", "rotate", "password_style", "password_words"} {
		if gjson.GetBytes(meta, k).Exists() {
			return authn.Principal{}, "invalid_arg", fmt.Sprintf(
				"`%s` is no longer accepted: an account holds no password. Give the account a `principal` here, "+
					"then issue its password with txco://credential/create", k)
		}
	}
	if ids == nil {
		return authn.Principal{}, "disabled", "no identity store on this node (auth.db failed to open at boot; see the chassis log)"
	}
	tenantID, err := lookupTenantID(ctx, snap, tenantSlug)
	if err != nil {
		return authn.Principal{}, "no_tenant", err.Error()
	}

	var p authn.Principal
	if raw := gjson.GetBytes(meta, "principal").String(); raw != "" {
		if p, err = authn.ParsePrincipal(raw); err != nil {
			return authn.Principal{}, "invalid_arg", err.Error()
		}
	} else {
		b, err := ids.LookupBinding(ctx, tenantID, authn.BindEmail, "", username)
		switch {
		case errors.Is(err, authn.ErrNotFound):
			return authn.Principal{}, "invalid_arg", "`principal` is required: who this account's username signs in as " +
				"(user:usr_… from txco://user/create, or <kind>:<name> such as pony:paris)"
		case err != nil:
			return authn.Principal{}, "store", err.Error()
		}
		p = b.Principal
	}

	_, _, err = ids.BindPrincipal(ctx, tenantID, processor.StackScope(ctx), p,
		authn.NewBinding{Kind: authn.BindEmail, Subject: username})
	switch {
	case err == nil:
		return p, "", ""
	case errors.Is(err, authn.ErrBound):
		return authn.Principal{}, "username_bound", fmt.Sprintf("%s already signs in as another principal", username)
	case errors.Is(err, authn.ErrNotOwner):
		return authn.Principal{}, "not_owner", authorMessage(err)
	case errors.Is(err, authn.ErrNotFound):
		return authn.Principal{}, "invalid_arg", fmt.Sprintf("principal %s not found: create the user first (txco://user/create)", p.ID)
	case errors.Is(err, authn.ErrNoStack):
		return authn.Principal{}, "no_stack", authorMessage(err)
	case errors.Is(err, authn.ErrInvalid):
		return authn.Principal{}, "invalid_arg", authorMessage(err)
	}
	return authn.Principal{}, "store", err.Error()
}

// lookupTenantID resolves the pinned tenant slug to the tenant id the
// identity tables key on, against the mirror snapshot.
func lookupTenantID(ctx context.Context, snap func() *sql.DB, slug string) (string, error) {
	if slug == "" {
		return "", errors.New("no tenant in request scope")
	}
	var db *sql.DB
	if snap != nil {
		db = snap()
	}
	if db == nil {
		return "", errors.New("no tenant directory on this node")
	}
	var id string
	err := db.QueryRowContext(ctx, `SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug).Scan(&id)
	if err != nil || id == "" {
		return "", fmt.Errorf("tenant %q not found", slug)
	}
	return id, nil
}
