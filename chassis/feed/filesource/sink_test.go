package filesource

import (
	"context"
	"testing"

	_ "github.com/loremlabs/thanks-computer/chassis/feed/nop"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

func TestFileSinkAssignsMonotonicControlVersion(t *testing.T) {
	dir := t.TempDir()
	sink, err := feed.OpenSink("file", feed.SourceConfig{FileDir: dir})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if sink.Name() != "file" {
		t.Errorf("name=%q", sink.Name())
	}

	in := []controlevent.Event{
		{EventID: "evt-1", Type: controlevent.TypeTenantCreated},
		{EventID: "evt-2", Type: controlevent.TypeTenantCreated},
		{EventID: "evt-3", Type: controlevent.TypeTenantCreated},
	}
	out := make([]controlevent.Event, len(in))
	for i, e := range in {
		got, err := sink.Append(context.Background(), e)
		if err != nil {
			t.Fatalf("append[%d]: %v", i, err)
		}
		out[i] = got
	}
	if out[0].ControlVersion != 1 || out[1].ControlVersion != 2 || out[2].ControlVersion != 3 {
		t.Errorf("expected control_version 1,2,3 got %d,%d,%d",
			out[0].ControlVersion, out[1].ControlVersion, out[2].ControlVersion)
	}
}

func TestFileSinkIdempotentRepublish(t *testing.T) {
	dir := t.TempDir()
	sink, _ := feed.OpenSink("file", feed.SourceConfig{FileDir: dir})

	ev := controlevent.Event{EventID: "evt-dup", Type: controlevent.TypeTenantCreated}
	first, err := sink.Append(context.Background(), ev)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if first.ControlVersion == 0 {
		t.Fatalf("first append assigned zero control_version")
	}
	// Republish same EventID — must return SAME ControlVersion, NOT
	// burn a fresh slot.
	second, err := sink.Append(context.Background(), ev)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}
	if second.ControlVersion != first.ControlVersion {
		t.Errorf("republish got control_version %d, want %d (idempotent)",
			second.ControlVersion, first.ControlVersion)
	}
	// A different event_id then gets the next slot.
	other, err := sink.Append(context.Background(), controlevent.Event{
		EventID: "evt-different", Type: controlevent.TypeTenantCreated,
	})
	if err != nil {
		t.Fatalf("third append: %v", err)
	}
	if other.ControlVersion != first.ControlVersion+1 {
		t.Errorf("third append control_version=%d, want %d",
			other.ControlVersion, first.ControlVersion+1)
	}
}

func TestFileSinkResumesFromExistingFiles(t *testing.T) {
	dir := t.TempDir()
	// Prime the dir with an existing event written by a "previous" sink
	// instance (simulating a chassis restart).
	prev, _ := feed.OpenSink("file", feed.SourceConfig{FileDir: dir})
	if _, err := prev.Append(context.Background(), controlevent.Event{
		EventID: "evt-old-1", Type: controlevent.TypeTenantCreated,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := prev.Append(context.Background(), controlevent.Event{
		EventID: "evt-old-2", Type: controlevent.TypeTenantCreated,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fresh sink picks up where the previous left off.
	fresh, err := feed.OpenSink("file", feed.SourceConfig{FileDir: dir})
	if err != nil {
		t.Fatalf("open fresh sink: %v", err)
	}
	got, err := fresh.Append(context.Background(), controlevent.Event{
		EventID: "evt-after-restart", Type: controlevent.TypeTenantCreated,
	})
	if err != nil {
		t.Fatalf("append after restart: %v", err)
	}
	if got.ControlVersion != 3 {
		t.Errorf("restart resume: control_version=%d, want 3", got.ControlVersion)
	}
}

func TestNopSinkAcceptsButDiscards(t *testing.T) {
	sink, err := feed.OpenSink("nop", feed.SourceConfig{})
	if err != nil {
		t.Fatalf("open nop sink: %v", err)
	}
	got, err := sink.Append(context.Background(), controlevent.Event{
		EventID: "evt-1", Type: controlevent.TypeTenantCreated,
	})
	if err != nil {
		t.Errorf("nop sink should not error: %v", err)
	}
	if got.ControlVersion != 0 {
		t.Errorf("nop sink should leave control_version=0, got %d", got.ControlVersion)
	}
}
