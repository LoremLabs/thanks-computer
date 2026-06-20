package admin

// Room inlet — `POST /v1/tenants/{tenant}/rooms/{room}/messages` and the live
// feed `GET .../rooms/{room}/stream` (SSE). A room is a durable shared context;
// a message becomes a normal TxCo event (`@src == "room"`) that enters the same
// processor/rule machinery as web, mail, and cron, routed to the tenant's
// `_room` stack (see detectTenantBody). There is no privileged "assistant
// path": the event is recorded in the trace log (events-as-history) and
// inherits the same tenant/capability/fuel/audit checks as any inlet.
//
// v1 is tenant-scoped: any actor with membership in the URL tenant — enforced
// by the tenant resolver middleware, which replaces capabilities with this
// tenant's membership caps (no membership → denied) — may post and read. Per-
// room ACLs and visibility are a later refinement.
//
// Both the user's message and the room stack's reply (`.text`) are published to
// the in-process hub so live SSE subscribers render them. The hub + ring buffer
// are single-node; fleet fan-out is an overlay seam (NATS), deferred.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/room"
)

const (
	// roomReplyTimeout bounds how long the inlet waits for the room stack to
	// answer synchronously. Generous because a room stack may call a model.
	roomReplyTimeout = 60 * time.Second
	// roomRingSize is how many recent events per room the hub keeps for SSE
	// backfill (durable history lives in the trace log).
	roomRingSize = 50
	// roomReplyActor attributes a stack's reply in the feed. The stack is the
	// responder; per-stack/agent identities are a later refinement.
	roomReplyActor = "_room"
	// roomStreamKeepalive pings idle SSE connections so proxies don't reap them.
	roomStreamKeepalive = 25 * time.Second
)

type postRoomMessageRequest struct {
	Text string `json:"text"`
}

type roomMessageDTO struct {
	MessageID string `json:"message_id"`
	Room      string `json:"room"`
	Actor     string `json:"actor"`
	// Text is the room stack's reply, if anything answered (empty otherwise).
	Text string `json:"text,omitempty"`
}

// roomEventPayload builds the `@src=="room"` event envelope JSON. tenantSlug is
// the trusted slug from the authenticated path; detectTenantBody routes it to
// the tenant's `_room/0`. Kept separate from the handler so the envelope shape
// is unit-testable without a live processor.
func roomEventPayload(tenantSlug, room, actor, msgID, text string) string {
	p, _ := sjson.Set("", "_txc.src", "room")
	p, _ = sjson.Set(p, "_txc.room.tenant", tenantSlug)
	p, _ = sjson.Set(p, "_txc.room.name", room)
	p, _ = sjson.Set(p, "_txc.room.actor", actor)
	p, _ = sjson.Set(p, "_txc.room.message_id", msgID)
	p, _ = sjson.Set(p, "_txc.room.text", text)
	p, _ = sjson.Set(p, "_txc.room.source.kind", "cli")
	p, _ = sjson.Set(p, "_txc.room.source.command", "thanks")
	return p
}

// handlePostRoomMessage converts one message into a `@src=="room"` event, runs
// it through the processor for the URL tenant, and returns the stack's reply.
func (c *Controller) handlePostRoomMessage(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_missing", nil)
		return
	}
	roomName := strings.TrimSpace(mux.Vars(r)["room"])
	if roomName == "" {
		writeJSONError(w, http.StatusBadRequest, "room_required", nil)
		return
	}

	var req postRoomMessageRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeJSONError(w, http.StatusBadRequest, "text_required", nil)
		return
	}

	// The actor is taken from the verified request — never the client. Empty in
	// open auth mode (no signed identity); attribute those to "anonymous".
	actor := ac.ActorID
	if actor == "" {
		actor = "anonymous"
	}
	msgID := "msg_" + hxid.NewTimeSort().String()

	// The user's message joins the room feed immediately, before the stack
	// replies, so live subscribers see it land.
	c.hub.Publish(ac.TenantSlug, roomName, actor, text, msgID)

	payload := roomEventPayload(ac.TenantSlug, roomName, actor, msgID, text)
	ctx, cancel := context.WithTimeout(r.Context(), roomReplyTimeout)
	defer cancel()
	// Buffered so the processor's single write never blocks even if we've
	// already returned on a timeout.
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(ctx, payload, resCh, "room")

	select {
	case c.pu.Bus <- envelope:
	case <-ctx.Done():
		writeJSONError(w, http.StatusServiceUnavailable, "room_busy", map[string]any{"err": ctx.Err().Error()})
		return
	}

	var reply string
	select {
	case res := <-resCh:
		reply = strings.TrimSpace(gjson.Get(res.Raw, "text").String())
	case <-ctx.Done():
		writeJSONError(w, http.StatusGatewayTimeout, "room_timeout", map[string]any{"err": ctx.Err().Error()})
		return
	}

	// The stack's reply joins the feed too, attributed to the responder.
	if reply != "" {
		c.hub.Publish(ac.TenantSlug, roomName, roomReplyActor, reply, "")
	}
	writeJSON(w, http.StatusOK, roomMessageDTO{
		MessageID: msgID,
		Room:      roomName,
		Actor:     actor,
		Text:      reply,
	})
}

// handleRoomStream is the live room feed: an SSE stream of room Events. It
// replays the recent ring buffer, then streams live until the client
// disconnects (request-context cancel). Each event is one `data:` line of the
// room.Event JSON, with the per-room sequence as the SSE `id:`.
func (c *Controller) handleRoomStream(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_missing", nil)
		return
	}
	roomName := strings.TrimSpace(mux.Vars(r)["room"])
	if roomName == "" {
		writeJSONError(w, http.StatusBadRequest, "room_required", nil)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "stream_unsupported", nil)
		return
	}

	ch, backfill, unsub := c.hub.Subscribe(ac.TenantSlug, roomName)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // tell proxies not to buffer the stream
	w.WriteHeader(http.StatusOK)

	send := func(ev room.Event) bool {
		b, err := json.Marshal(ev)
		if err != nil {
			return true // skip a malformed event, keep the stream open
		}
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, ev := range backfill {
		if !send(ev) {
			return
		}
	}
	// A comment line flushes the headers + opens the stream client-side even
	// before the first live event.
	fmt.Fprint(w, ": open\n\n")
	flusher.Flush()

	ka := time.NewTicker(roomStreamKeepalive)
	defer ka.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !send(ev) {
				return
			}
		case <-ka.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
