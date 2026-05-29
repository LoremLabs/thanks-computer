package filesource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/loremlabs/thanks-computer/chassis/feed/nop"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

func write(t *testing.T, dir, name string, ev controlevent.Event) {
	t.Helper()
	b, _ := json.Marshal(ev)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestFilesourceOrderingAndSince(t *testing.T) {
	dir := t.TempDir()
	s, err := feed.Open("file", feed.SourceConfig{FileDir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if s.Name() != "file" {
		t.Errorf("name=%q", s.Name())
	}
	// Out-of-order on disk; expect ascending by ControlVersion.
	write(t, dir, "c.json", controlevent.Event{Type: controlevent.TypeTenantCreated, ControlVersion: 30})
	write(t, dir, "a.json", controlevent.Event{Type: controlevent.TypeTenantCreated, ControlVersion: 10})
	write(t, dir, "b.json", controlevent.Event{Type: controlevent.TypeTenantCreated, ControlVersion: 20})
	write(t, dir, "notjson.txt", controlevent.Event{}) // ignored

	got, err := s.Poll(context.Background(), 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 3 || got[0].ControlVersion != 10 || got[1].ControlVersion != 20 || got[2].ControlVersion != 30 {
		t.Fatalf("ordering wrong: %+v", got)
	}

	// since filters strictly greater.
	got, _ = s.Poll(context.Background(), 20)
	if len(got) != 1 || got[0].ControlVersion != 30 {
		t.Fatalf("since filter wrong: %+v", got)
	}
}

func TestFilesourceMalformedIsLoud(t *testing.T) {
	dir := t.TempDir()
	s, _ := feed.Open("file", feed.SourceConfig{FileDir: dir})
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(context.Background(), 0); err == nil {
		t.Fatalf("expected error on malformed event file")
	}
}

func TestNopRegistered(t *testing.T) {
	s, err := feed.Open("nop", feed.SourceConfig{})
	if err != nil {
		t.Fatalf("open nop: %v", err)
	}
	evs, err := s.Poll(context.Background(), 0)
	if err != nil || evs != nil {
		t.Errorf("nop should yield nothing: %v %v", evs, err)
	}
}
