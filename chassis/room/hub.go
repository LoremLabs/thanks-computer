// Package room is an in-process pub/sub for room timelines: the room inlet
// publishes messages to a (tenant, room); SSE subscribers receive them live,
// plus a short backfill of recent ones.
//
// Single-node by design — fleet fan-out across chassis nodes is an overlay
// seam (the NATS feed/trace-subscriber backends), deferred. Durable history is
// the event/trace log (events-as-history); the ring buffer here is only a
// live-backfill cache, not the system of record.
package room

import "sync"

// Event is one message in a room's timeline, as published to subscribers.
type Event struct {
	Seq       int64  `json:"seq"`
	Room      string `json:"room"`
	Actor     string `json:"actor"`
	Text      string `json:"text"`
	MessageID string `json:"message_id,omitempty"`
}

// Hub fans out room Events to live subscribers and keeps a small per-room ring
// buffer of recent Events for subscriber backfill. Safe for concurrent use.
//
// With a Relay attached (SetRelay), locally-published Events also fan out to
// other fleet nodes, and Events from other nodes arrive via Deliver. Without
// one, the Hub is purely in-process (single node).
type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*roomState
	ringSize int
	// relay is set once at boot, before serving; nil = in-process only.
	relay Relay
}

type roomState struct {
	seq     int64
	recent  []Event
	subs    map[int]chan Event
	nextSub int
}

// NewHub returns a Hub keeping the last ringSize Events per room for backfill
// (ringSize <= 0 defaults to 50).
func NewHub(ringSize int) *Hub {
	if ringSize <= 0 {
		ringSize = 50
	}
	return &Hub{rooms: map[string]*roomState{}, ringSize: ringSize}
}

func roomKey(tenant, room string) string { return tenant + "\x00" + room }

func (h *Hub) stateLocked(tenant, room string) *roomState {
	k := roomKey(tenant, room)
	rs := h.rooms[k]
	if rs == nil {
		rs = &roomState{subs: map[int]chan Event{}}
		h.rooms[k] = rs
	}
	return rs
}

// SetRelay attaches a cross-node relay so locally-published Events fan out to
// other fleet nodes. Call once at boot, before serving; nil keeps the Hub
// purely in-process.
func (h *Hub) SetRelay(r Relay) { h.relay = r }

// Publish records a locally-created Event for (tenant, room) — assigning a
// per-room sequence, ringing it, and fanning it out to local subscribers — then
// relays it to other fleet nodes when a Relay is attached. Returns the stored
// Event.
func (h *Hub) Publish(tenant, room, actor, text, messageID string) Event {
	ev := h.deliverLocked(tenant, room, Event{Actor: actor, Text: text, MessageID: messageID}, true)
	if h.relay != nil {
		h.relay.Publish(tenant, room, ev)
	}
	return ev
}

// Deliver injects an Event received from ANOTHER fleet node into this Hub's
// local subscribers + ring. It is the callback handed to a Relay. To bound
// memory it only delivers to rooms this node already has state for (something
// subscribed or published here) — a node with no local interest in a room drops
// remote Events for it rather than materializing unbounded per-room state. It
// re-sequences locally and does NOT relay (no loop).
func (h *Hub) Deliver(tenant, room string, ev Event) {
	h.deliverLocked(tenant, room, ev, false)
}

// deliverLocked is the shared ring+fanout core. create=true materializes room
// state on demand (the local Publish path); create=false delivers only to
// already-known rooms (the remote Deliver path).
func (h *Hub) deliverLocked(tenant, room string, ev Event, create bool) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var rs *roomState
	if create {
		rs = h.stateLocked(tenant, room)
	} else if rs = h.rooms[roomKey(tenant, room)]; rs == nil {
		return ev // no local interest in this room
	}
	rs.seq++
	ev.Seq = rs.seq
	ev.Room = room
	rs.recent = append(rs.recent, ev)
	if len(rs.recent) > h.ringSize {
		rs.recent = rs.recent[len(rs.recent)-h.ringSize:]
	}
	for _, ch := range rs.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop rather than stall the publisher
		}
	}
	return ev
}

// Subscribe registers a live subscriber for (tenant, room). It returns the live
// Event channel, a snapshot of the recent ring buffer (oldest first), and an
// unsubscribe func the caller MUST invoke when done (it removes the subscriber
// and closes the channel; calling it more than once is safe).
func (h *Hub) Subscribe(tenant, room string) (<-chan Event, []Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.stateLocked(tenant, room)
	id := rs.nextSub
	rs.nextSub++
	ch := make(chan Event, 64)
	rs.subs[id] = ch
	backfill := append([]Event(nil), rs.recent...)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if cur := h.rooms[roomKey(tenant, room)]; cur != nil {
				delete(cur.subs, id)
			}
			close(ch)
		})
	}
	return ch, backfill, unsub
}
