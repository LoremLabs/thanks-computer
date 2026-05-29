package usage

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ZapSink should emit exactly one "usage" entry carrying every field
// with the right type — this is the contract a downstream log consumer
// parses against.
func TestZapSinkWriteEvent(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	sink := NewZapSink(zap.New(core))

	sink.WriteEvent(UsageEvent{
		RID:        "hx_abc",
		Tenant:     "acme",
		Src:        "http",
		Stack:      "boot/0",
		DurationMS: 32,
		Status:     "ok",
		BytesIn:    1024,
		BytesOut:   8123,
	})

	all := logs.All()
	if len(all) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(all))
	}
	e := all[0]
	if e.Message != "usage" {
		t.Fatalf("want message %q, got %q", "usage", e.Message)
	}

	f := e.ContextMap()
	cases := map[string]any{
		"rid":         "hx_abc",
		"tenant":      "acme",
		"src":         "http",
		"stack":       "boot/0",
		"duration_ms": int64(32),
		"status":      "ok",
		"bytes_in":    int64(1024), // zap.Int is recorded as int64 by observer
		"bytes_out":   int64(8123),
	}
	for k, want := range cases {
		if got := f[k]; got != want {
			t.Errorf("field %q: want %v (%T), got %v (%T)", k, want, want, got, got)
		}
	}
}

// Empty/_sys tenant is logged as-is so unrouted traffic is still
// measured; the sink must not drop or substitute it.
func TestZapSinkEmptyTenant(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	sink := NewZapSink(zap.New(core))

	sink.WriteEvent(UsageEvent{RID: "hx_x", Tenant: "", Src: "http", Status: "error"})

	f := logs.All()[0].ContextMap()
	if f["tenant"] != "" {
		t.Fatalf("want empty tenant preserved, got %v", f["tenant"])
	}
	if f["status"] != "error" {
		t.Fatalf("want status error, got %v", f["status"])
	}
}

func TestZapSinkCloseNoop(t *testing.T) {
	sink := NewZapSink(zap.NewNop())
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("Close should be a no-op, got %v", err)
	}
}

// WriteEvent is called from per-request goroutines; concurrent calls
// must not race or lose entries.
func TestZapSinkConcurrent(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	sink := NewZapSink(zap.New(core))

	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			sink.WriteEvent(UsageEvent{RID: "r", Src: "http", Status: "ok"})
		}()
	}
	wg.Wait()

	if got := logs.Len(); got != n {
		t.Fatalf("want %d entries, got %d", n, got)
	}
}
