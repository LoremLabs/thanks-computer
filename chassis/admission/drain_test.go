package admission

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestDrainFlag(t *testing.T) {
	t.Cleanup(func() { SetDraining(false) })
	if IsDraining() {
		t.Fatal("drain should be off by default")
	}
	SetDraining(true)
	if !IsDraining() {
		t.Fatal("SetDraining(true) should enable drain")
	}
	SetDraining(false)
	if IsDraining() {
		t.Fatal("SetDraining(false) should disable drain")
	}
}

// DrainResponse stamps the transport-neutral marker (status 503). The
// per-protocol rendering (web 503+Retry-After, lmtp 451, tcp close) is
// tested in each personality, not here.
func TestDrainResponseMarker(t *testing.T) {
	out := DrainResponse(`{"_txc":{"src":"http"}}`)
	if status, reason, ok := Denied(out); !ok || status != 503 || reason != "draining" {
		t.Errorf("Denied = (%d,%q,%v), want (503,draining,true)", status, reason, ok)
	}
	// Neutral: admission must NOT shape transport-specific fields.
	if gjson.Get(out, "_txc.web.res.status").Exists() {
		t.Error("admission must not shape web response fields (that's the outlet's job)")
	}
}
