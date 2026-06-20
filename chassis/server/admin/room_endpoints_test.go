package admin

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestRoomEventPayload pins the room event envelope shape: src=room, the
// trusted tenant slug, and the room/actor/message fields detectTenantBody and
// the room stack read.
func TestRoomEventPayload(t *testing.T) {
	p := roomEventPayload("acme", "support", "actor_1", "msg_abc", "why did ticket 184 fail?")
	checks := map[string]string{
		"_txc.src":                 "room",
		"_txc.room.tenant":         "acme",
		"_txc.room.name":           "support",
		"_txc.room.actor":          "actor_1",
		"_txc.room.message_id":     "msg_abc",
		"_txc.room.text":           "why did ticket 184 fail?",
		"_txc.room.source.kind":    "cli",
		"_txc.room.source.command": "thanks",
	}
	for path, want := range checks {
		if got := gjson.Get(p, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}
