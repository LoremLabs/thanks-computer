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
type Hub struct {
	mu       sync.Mutex
	rooms    map[string]*roomState
	ringSize int
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

// Publish appends an Event to (tenant, room): it assigns a per-room sequence,
// records it in the ring buffer, and fans it out to live subscribers
// (non-blocking — a subscriber whose buffer is full drops this Event rather
// than stalling the publisher). Returns the stored Event.
func (h *Hub) Publish(tenant, room, actor, text, messageID string) Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.stateLocked(tenant, room)
	rs.seq++
	ev := Event{Seq: rs.seq, Room: room, Actor: actor, Text: text, MessageID: messageID}
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
