package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/allowance"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// allowance.go holds the txco://allowance/* ops: a tenant's own fuel budgets
// inside its tenant budget (chassis/allowance). The tenant comes from the
// request pin (processor.TenantScope), so a stack only ever sees and manages
// its own tenant's allowances. Results land under `_allowance` by default.
//
//   enter   — put this request in an allowance; refused (429) if it is spent
//   get     — one allowance's current window (default: the one entered)
//   set     — create or replace {name, fuel, per}
//   delete  — remove a definition (its counters expire on their own)
//   list    — the definitions, a page at a time

func allowanceErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "allowance-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}

func allowanceFail(err error) (event.Payload, error) {
	return allowanceErr(err.Error()), err
}

func allowanceTenant(ctx context.Context, store *allowance.Store) (string, error) {
	if store == nil {
		return "", errors.New("allowance: no KV store configured")
	}
	t := processor.TenantScope(ctx)
	if t == "" {
		return "", errors.New("allowance: no tenant in request scope")
	}
	return t, nil
}

// statusJSON renders a window's status. remaining is present only for a
// defined allowance: an undefined one is metered, not limited.
func statusJSON(st allowance.Status) *jsonx.Builder {
	o := jsonx.NewObject()
	o.Set("name", st.Name)
	o.Set("defined", st.Defined)
	o.Set("per", string(st.Per))
	o.Set("used", st.Used)
	if st.Defined {
		o.Set("fuel", st.Fuel)
		o.Set("remaining", st.Remaining())
	}
	if !st.ResetsAt.IsZero() {
		o.Set("resets_at", st.ResetsAt.UTC().Format(time.RFC3339))
	}
	return o
}

func allowanceResult(meta []byte, o *jsonx.Builder) event.Payload {
	resp, _ := sjson.SetRaw(`{}`, intoPath(meta, "_allowance"), o.String())
	return event.Payload{Raw: resp, Type: event.JSON}
}

// allowanceEnter puts the request in the allowance WITH `name`. Write-once:
// a request already in another allowance is an error. A spent window is not
// an op error — the op answers {denied: true} and the request ends at its
// next budget check with 429 allowance_exhausted. If the store cannot be
// read the request still enters, metered but unchecked ({checked: false}):
// the tenant's own budget still applies, and an allowance outage should not
// take the tenant's traffic down with it.
func allowanceEnter(ctx context.Context, store *allowance.Store, in []byte) (event.Payload, error) {
	tenant, err := allowanceTenant(ctx, store)
	if err != nil {
		return allowanceFail(err)
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "name").String()
	if !allowance.ValidName(name) {
		return allowanceFail(fmt.Errorf("allowance/enter: invalid or missing `name` %q", name))
	}
	st, serr := store.Status(ctx, tenant, name)
	checked := serr == nil
	if !checked {
		st = allowance.Status{Name: name, Per: allowance.DefaultPeriod}
	}
	denied, err := processor.EnterAllowance(ctx, processor.AllowanceCheck{
		Name: name, Defined: st.Defined, Fuel: st.Fuel, Used: st.Used, ResetsAt: st.ResetsAt,
	})
	if err != nil {
		return allowanceFail(fmt.Errorf("allowance/enter: %w", err))
	}
	o := statusJSON(st)
	o.Set("entered", true)
	o.Set("denied", denied)
	o.Set("checked", checked)
	return allowanceResult(meta, o), nil
}

// allowanceGet reads one allowance's current window: WITH `name`, else the
// allowance the request entered.
func allowanceGet(ctx context.Context, store *allowance.Store, in []byte) (event.Payload, error) {
	tenant, err := allowanceTenant(ctx, store)
	if err != nil {
		return allowanceFail(err)
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "name").String()
	if name == "" {
		name = processor.AllowanceScope(ctx)
	}
	if name == "" {
		return allowanceFail(errors.New("allowance/get: missing `name` and the request entered no allowance"))
	}
	st, err := store.Status(ctx, tenant, name)
	if err != nil {
		return allowanceFail(fmt.Errorf("allowance/get: %w", err))
	}
	return allowanceResult(meta, statusJSON(st)), nil
}

// allowanceSet creates or replaces WITH `name`, `fuel` and `per` (hour, day
// or month; default day).
func allowanceSet(ctx context.Context, store *allowance.Store, in []byte) (event.Payload, error) {
	tenant, err := allowanceTenant(ctx, store)
	if err != nil {
		return allowanceFail(err)
	}
	meta := []byte(operation.MetaFromContext(ctx))
	per := gjson.GetBytes(meta, "per").String()
	if per == "" {
		per = string(allowance.DefaultPeriod)
	}
	d := allowance.Def{
		Name: gjson.GetBytes(meta, "name").String(),
		Fuel: gjson.GetBytes(meta, "fuel").Int(),
		Per:  allowance.Period(per),
	}
	if err := store.Set(ctx, tenant, d); err != nil {
		return allowanceFail(fmt.Errorf("allowance/set: %w", err))
	}
	o := jsonx.NewObject()
	o.Set("name", d.Name)
	o.Set("fuel", d.Fuel)
	o.Set("per", string(d.Per))
	return allowanceResult(meta, o), nil
}

// allowanceDelete removes the definition WITH `name` (a missing one is a
// success).
func allowanceDelete(ctx context.Context, store *allowance.Store, in []byte) (event.Payload, error) {
	tenant, err := allowanceTenant(ctx, store)
	if err != nil {
		return allowanceFail(err)
	}
	meta := []byte(operation.MetaFromContext(ctx))
	if err := store.Delete(ctx, tenant, gjson.GetBytes(meta, "name").String()); err != nil {
		return allowanceFail(fmt.Errorf("allowance/delete: %w", err))
	}
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
}

// allowanceList writes {allowances:[{name, fuel, per}], next, count}: at most
// `limit` definitions after the `after` cursor, in name order.
func allowanceList(ctx context.Context, store *allowance.Store, in []byte) (event.Payload, error) {
	tenant, err := allowanceTenant(ctx, store)
	if err != nil {
		return allowanceFail(err)
	}
	meta := []byte(operation.MetaFromContext(ctx))
	defs, next, err := store.List(ctx, tenant,
		gjson.GetBytes(meta, "after").String(), int(gjson.GetBytes(meta, "limit").Int()))
	if err != nil {
		return allowanceFail(fmt.Errorf("allowance/list: %w", err))
	}
	rows := "[]"
	for i, d := range defs {
		row := jsonx.NewObject()
		row.Set("name", d.Name)
		row.Set("fuel", d.Fuel)
		row.Set("per", string(d.Per))
		rows, _ = sjson.SetRaw(rows, fmt.Sprint(i), row.String())
	}
	o := jsonx.NewObject()
	o.SetRaw("allowances", rows)
	o.Set("next", next)
	o.Set("count", len(defs))
	return allowanceResult(meta, o), nil
}
