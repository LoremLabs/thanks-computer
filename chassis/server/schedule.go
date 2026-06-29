package server

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/scheduled"
)

// schedule.go holds the handler body for txco://schedule — the op-writable
// surface over the scheduled_events store (chassis/scheduled). It enqueues a
// future event ("run this payload, not before schedule_at"), reschedules a
// still-pending one, or cancels one.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx) (the
// request-pinned tenant, NOT the mutable _txc.tenant), so an event always
// fires for the tenant whose pipeline enqueued it. The scheduled personality
// later re-tenants the stored payload into that tenant's `_scheduled/0` stack.
//
// WITH params:
//   idempotency_key (req) — dedup + cancel handle, scoped per tenant. Re-running
//                           with the same key while PENDING reschedules in place;
//                           a fired key is spent.
//   schedule_at     (req unless cancel) — RFC3339 (e.g. 2026-06-29T18:42:00Z);
//                           the event never fires before this instant.
//   payload         — JSON object stamped onto _txc.scheduled.payload at fire.
//   cancel          — when true, delete the pending event for idempotency_key.
//
// Output (under the `_schedule` private key, dropped from the web projection):
// {id, scheduled_at} on enqueue, {cancelled:bool} on cancel, {error} on failure.

func scheduleErr(msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, "_schedule.error", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func scheduleOp(ctx context.Context, store *scheduled.Store, in []byte) (event.Payload, error) {
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		e := "schedule: no tenant in request scope"
		return scheduleErr(e), errors.New(e)
	}
	meta := []byte(operation.MetaFromContext(ctx))

	idKey := gjson.GetBytes(meta, "idempotency_key").String()
	if idKey == "" {
		e := "schedule: missing `idempotency_key`"
		return scheduleErr(e), errors.New(e)
	}

	if gjson.GetBytes(meta, "cancel").Bool() {
		cancelled, err := store.Cancel(ctx, tenant, idKey)
		if err != nil {
			return scheduleErr(err.Error()), err
		}
		raw, _ := sjson.Set(`{}`, "_schedule.cancelled", cancelled)
		return event.Payload{Raw: raw, Type: event.JSON}, nil
	}

	atStr := gjson.GetBytes(meta, "schedule_at").String()
	if atStr == "" {
		e := "schedule: missing `schedule_at`"
		return scheduleErr(e), errors.New(e)
	}
	at, perr := time.Parse(time.RFC3339, atStr)
	if perr != nil {
		e := "schedule: `schedule_at` must be RFC3339 (e.g. 2026-06-29T18:42:00Z): " + perr.Error()
		return scheduleErr(e), errors.New(e)
	}

	var payload json.RawMessage
	if p := gjson.GetBytes(meta, "payload"); p.Exists() {
		payload = json.RawMessage(p.Raw)
	}

	id, err := store.Enqueue(ctx, tenant, idKey, at, payload)
	if err != nil {
		return scheduleErr(err.Error()), err
	}
	raw, _ := sjson.Set(`{}`, "_schedule.id", id)
	raw, _ = sjson.Set(raw, "_schedule.scheduled_at", at.UTC().Format(time.RFC3339))
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}
