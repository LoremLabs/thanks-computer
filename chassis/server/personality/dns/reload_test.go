package dns

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestOnReloadNoDeadlock guards the dbcache deadlock regression. The
// OnReload hook runs INSIDE dbcache.Reload while dbc.Mu is held, so it
// must rebuild the zone snapshot from the mirror handed to it, NOT from
// dbc.Snapshot() (which relocks dbc.Mu and hangs the whole chassis). The
// buggy version blocked here forever; the bound turns a regression into
// a failure instead of a hang.
func TestOnReloadNoDeadlock(t *testing.T) {
	src := newTestDB(t)
	seedPatternZone(t, src, patTenant, "ops.example.com", fixedTS)
	seedSettings(t, src, "ns1.txco.io", "203.0.113.10", "mx.txco.io")

	dbc, err := dbcache.New(config.Config{}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("dbcache.New: %v", err)
	}
	if err := dbc.Reload(); err != nil { // populate the mirror from src
		t.Fatalf("initial reload: %v", err)
	}

	c := &DNSController{
		pu: &processor.Unit{Logger: zap.NewNop(), Dbc: dbc},
		synthCfg: SynthConfig{
			Nameservers: []string{"ns1.txco.io"},
			EdgeIPs:     []string{"203.0.113.10"},
			MXHost:      "mx.txco.io",
		},
	}
	c.installReload() // initial build (Snapshot, outside Reload) + chains OnReload

	// This reload runs the dns OnReload under dbc.Mu — the deadlock path.
	done := make(chan error, 1)
	go func() { done <- dbc.Reload() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reload returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dbc.Reload deadlocked — dns OnReload likely called Snapshot() under the held lock")
	}

	if snap := c.snap.Load(); snap == nil || snap.byOrigin("ops.example.com") == nil {
		t.Fatal("zone snapshot not rebuilt after reload")
	}
}
