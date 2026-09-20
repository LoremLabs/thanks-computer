package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// txco://ipp/printer registers a printer: the label in its URL, the
// principal that may print to it, and the name a client shows for it. The
// `ipp` personality reads the row on every request; a label with no active
// row does not exist (docs/advanced/protocols/ipp.md).
//
//	WITH printer, principal, display_name?, status? (active|disabled), into?
//	WITH printer, delete = true
//
// Idempotent. A printer holds no password: its principal signs in with a
// credential from txco://credential/create whose scopes cover
// `ipp:<printer>:print`.
//
// The creator-stack rule covers printers (authn.RequireOwner): the calling
// stack must manage the principal, which must already exist — so a printer
// cannot be pointed at a name another stack could later claim — and changing
// or deleting a printer needs the principal it has now as well.
type ippDeps struct {
	store *chipp.Store // nil ⇒ txco_ipp_disabled
	ids   *authn.Store
	snap  func() *sql.DB
}

func ippErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", "txco_ipp_"+code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// ippOwnerErr maps a RequireOwner error to an op code and message.
func ippOwnerErr(into string, p authn.Principal, err error) event.Payload {
	switch {
	case errors.Is(err, authn.ErrNotOwner):
		return ippErr(into, "not_owner", authorMessage(err))
	case errors.Is(err, authn.ErrNotFound):
		return ippErr(into, "not_found", fmt.Sprintf(
			"principal %s not found: create the user, or give the principal a username or a credential, first", p.ID))
	case errors.Is(err, authn.ErrNoStack):
		return ippErr(into, "no_stack", authorMessage(err))
	case errors.Is(err, authn.ErrInvalid):
		return ippErr(into, "invalid_arg", authorMessage(err))
	}
	return ippErr(into, "store", err.Error())
}

func ippPrinter(ctx context.Context, d ippDeps, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_printer")
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return ippErr(into, "no_tenant", "no tenant in request scope"), nil
	}
	if d.store == nil {
		return ippErr(into, "disabled", "no printer store on this node (the ipp personality is off, or its store failed to open at boot)"), nil
	}
	if d.ids == nil {
		return ippErr(into, "disabled", "no identity store on this node (auth.db failed to open at boot; see the chassis log)"), nil
	}
	label := strings.TrimSpace(gjson.GetBytes(meta, "printer").String())
	if !chipp.ValidLabel(label) {
		return ippErr(into, "invalid_arg", "`printer` must be a printer label: lowercase letters, digits, `.`, `_` and `-`, "+
			"starting and ending with a letter or digit, at most 64 characters — it is the last segment of the printer's URL"), nil
	}
	tenantID, err := lookupTenantID(ctx, d.snap, tenant)
	if err != nil {
		return ippErr(into, "no_tenant", err.Error()), nil
	}
	stack := processor.StackScope(ctx)

	// The printer as it is now. Whoever changes or removes it must manage
	// the principal it is granted to.
	existing, err := d.store.GetPrinter(ctx, tenant, label)
	found := err == nil
	if err != nil && !errors.Is(err, chipp.ErrPrinterNotFound) {
		return ippErr(into, "store", err.Error()), nil
	}
	if found {
		if cur, perr := authn.ParsePrincipal(existing.PrincipalID); perr == nil {
			if _, err := d.ids.RequireOwner(ctx, tenantID, stack, cur); err != nil && !errors.Is(err, authn.ErrNotFound) {
				return ippOwnerErr(into, cur, err), nil
			}
		}
	}

	if gjson.GetBytes(meta, "delete").Bool() {
		deleted := false
		if found {
			if deleted, err = d.store.DeletePrinter(ctx, tenant, label); err != nil {
				return ippErr(into, "store", err.Error()), nil
			}
		}
		out := jsonx.NewObject()
		out.Set(into+".printer", label)
		out.Set(into+".deleted", deleted)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}

	raw := strings.TrimSpace(gjson.GetBytes(meta, "principal").String())
	if raw == "" {
		return ippErr(into, "invalid_arg", "`principal` is required: who may print to this printer "+
			"(user:usr_… from txco://user/create, or <kind>:<name> such as pony:paris)"), nil
	}
	p, err := authn.ParsePrincipal(raw)
	if err != nil {
		return ippErr(into, "invalid_arg", authorMessage(err)), nil
	}
	base, err := d.ids.RequireOwner(ctx, tenantID, stack, p)
	if err != nil {
		return ippOwnerErr(into, p, err), nil
	}
	status := strings.TrimSpace(gjson.GetBytes(meta, "status").String())
	switch status {
	case "", chipp.PrinterActive, chipp.PrinterDisabled:
	default:
		return ippErr(into, "invalid_arg", "`status` must be \"active\" or \"disabled\""), nil
	}

	row, created, err := d.store.UpsertPrinter(ctx, chipp.Printer{
		Tenant: tenant, Label: label, PrincipalID: p.ID, Status: status, CreatedBy: base,
		DisplayName: strings.TrimSpace(gjson.GetBytes(meta, "display_name").String()),
	})
	if err != nil {
		return ippErr(into, "store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".printer", row.Label)
	out.Set(into+".principal", row.PrincipalID)
	out.Set(into+".display_name", row.DisplayName)
	out.Set(into+".status", row.Status)
	out.Set(into+".created", created)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}
