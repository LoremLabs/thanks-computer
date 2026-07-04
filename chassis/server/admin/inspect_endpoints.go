package admin

// Inspect inlet — `POST /v1/tenants/{tenant}/inspect`. Where `txco trace`
// answers "what just happened?", inspect answers "what is the current state,
// and why?" — by asking the tenant's own ops. The request becomes a normal
// TxCo event (`@src == "inspect"`) routed to the tenant's `_inspect` stack
// (see detectTenantBody); inspector ops there gate on `@inspect.stack` /
// `@inspect.noun`, gather their domain state (kv/get, read-file, nano-ops),
// and answer with a structured card at the top-level `_inspect.card` envelope
// path:
//
//	{ "title": "...", "sections": [ { "title": "...",
//	  "rows": [["label", value], ...] } ], "raw": { ... } }
//
// The card rides OUTSIDE `_txc.*` on purpose: ops may write it freely, while
// the request context under `_txc.inspect.*` is chassis-stamped and protected
// by the default-closed `_txc` guard — an op cannot forge `@inspect.tenant`.
// Same trust model as the room inlet; a tenant with no `_inspect` stack (or no
// matching inspector) simply yields no card, reported as 404 no_inspector.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// inspectReplyTimeout bounds how long the inlet waits for an inspector to
// answer synchronously. Generous because an inspector may fan out several
// kv/read-file lookups (mirrors roomReplyTimeout).
const inspectReplyTimeout = 60 * time.Second

type inspectRequest struct {
	// Stack names the domain being asked about (e.g. "marketing") — which
	// inspector family should answer. It is a dispatch value for the
	// inspector ops' WHEN guards, not a routing target: the event always
	// runs the tenant's `_inspect` stack.
	Stack string `json:"stack"`
	// Noun is the object kind ("user", "company", ...). Empty selects the
	// stack's index inspector (the "what can I ask?" discovery card), when
	// one is authored.
	Noun string `json:"noun,omitempty"`
	// ID identifies the object (email, slug, ...). Rides in the body so
	// values with '/' or '@' need no path escaping.
	ID   string         `json:"id,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

type inspectResponseDTO struct {
	// Card is the inspector's answer, passed through verbatim.
	Card json.RawMessage `json:"card"`
}

// inspectEventPayload builds the `@src=="inspect"` event envelope JSON.
// tenantSlug is the trusted slug from the authenticated path; detectTenantBody
// routes it to the tenant's `_inspect/0`. Kept separate from the handler so
// the envelope shape is unit-testable without a live processor.
func inspectEventPayload(tenantSlug, stack, noun, id string, args map[string]any) string {
	p, _ := sjson.Set("", "_txc.src", "inspect")
	p, _ = sjson.Set(p, "_txc.inspect.tenant", tenantSlug)
	p, _ = sjson.Set(p, "_txc.inspect.stack", stack)
	p, _ = sjson.Set(p, "_txc.inspect.noun", noun)
	p, _ = sjson.Set(p, "_txc.inspect.id", id)
	if len(args) > 0 {
		p, _ = sjson.Set(p, "_txc.inspect.args", args)
	}
	return p
}

// handleInspect converts one inspect request into a `@src=="inspect"` event,
// runs it through the processor for the URL tenant, and returns the card the
// tenant's inspector produced.
func (c *Controller) handleInspect(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "inspect:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_missing", nil)
		return
	}

	var req inspectRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	req.Stack = strings.TrimSpace(req.Stack)
	if req.Stack == "" {
		writeJSONError(w, http.StatusBadRequest, "stack_required", nil)
		return
	}

	payload := inspectEventPayload(ac.TenantSlug, req.Stack, strings.TrimSpace(req.Noun), strings.TrimSpace(req.ID), req.Args)
	ctx, cancel := context.WithTimeout(r.Context(), inspectReplyTimeout)
	defer cancel()
	// Buffered so the processor's single write never blocks even if we've
	// already returned on a timeout.
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(ctx, payload, resCh, "inspect")

	select {
	case c.pu.Bus <- envelope:
	case <-ctx.Done():
		writeJSONError(w, http.StatusServiceUnavailable, "inspect_busy", map[string]any{"err": ctx.Err().Error()})
		return
	}

	select {
	case res := <-resCh:
		card := gjson.Get(res.Raw, "_inspect.card")
		if !card.Exists() {
			writeJSONError(w, http.StatusNotFound, "no_inspector", map[string]any{
				"hint": "no inspector answered — does the tenant have an _inspect stack with an op matching this stack/noun?",
			})
			return
		}
		writeJSON(w, http.StatusOK, inspectResponseDTO{Card: json.RawMessage(card.Raw)})
	case <-ctx.Done():
		writeJSONError(w, http.StatusGatewayTimeout, "inspect_timeout", map[string]any{"err": ctx.Err().Error()})
	}
}
