package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func relayStep(name string, in, out string) StepInfo {
	return StepInfo{Stack: "agent", Scope: 100, Name: name, Input: []byte(in), Output: []byte(out),
		StartedAt: time.Now(), FinishedAt: time.Now(), Status: "ok"}
}

// The relay holds what it is given until it knows the target, replays it in
// order, then forwards.
func TestRelayHoldsThenForwards(t *testing.T) {
	sink := &recordingSink{}
	tr := sink.Begin(RequestInfo{RID: "r"})

	r := NewRelay(1 << 20)
	r.Step(relayStep("one", `{}`, `{}`))
	r.Event(TimelineEvent{Event: "first"})
	r.Step(relayStep("two", `{}`, `{}`))
	if _, steps, events, _ := sink.snapshot(); steps != 0 || events != 0 {
		t.Fatalf("before FlushTo the target got %d steps, %d events", steps, events)
	}

	r.FlushTo(tr)
	r.Step(relayStep("three", `{}`, `{}`))
	r.Event(TimelineEvent{Event: "second"})

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var names []string
	for _, s := range sink.steps {
		names = append(names, s.Name)
	}
	if fmt.Sprint(names) != "[one two three]" {
		t.Errorf("steps = %v", names)
	}
	if len(sink.events) != 2 || sink.events[0].Event != "first" || sink.events[1].Event != "second" {
		t.Errorf("events = %+v", sink.events)
	}
}

func TestRelayCloseDropsAndNeverEnds(t *testing.T) {
	sink := &recordingSink{}
	tr := sink.Begin(RequestInfo{RID: "r"})

	r := NewRelay(1 << 20)
	r.FlushTo(tr)
	r.Step(relayStep("kept", `{}`, `{}`))
	r.End("ok", "", nil)
	r.Close()
	r.Step(relayStep("late", `{}`, `{}`))
	r.Event(TimelineEvent{Event: "late"})

	if _, steps, events, ends := sink.snapshot(); steps != 1 || events != 0 || ends != 0 {
		t.Errorf("steps=%d events=%d ends=%d, want 1 0 0", steps, events, ends)
	}
}

// A relay no trace wants, and a nil relay, are quiet.
func TestRelayWithNoTarget(t *testing.T) {
	r := NewRelay(1 << 20)
	r.Step(relayStep("held", `{}`, `{}`))
	r.FlushTo(nil)
	r.Step(relayStep("late", `{}`, `{}`))
	if len(r.buf) != 0 {
		t.Errorf("a relay with no target still holds %d records", len(r.buf))
	}

	var none *Relay
	none.Step(StepInfo{})
	none.Event(TimelineEvent{})
	none.FlushTo(nil)
	none.Close()
}

// Only the first target counts.
func TestRelayFlushToIsOnce(t *testing.T) {
	first, second := &recordingSink{}, &recordingSink{}
	r := NewRelay(1 << 20)
	r.Step(relayStep("one", `{}`, `{}`))
	r.FlushTo(first.Begin(RequestInfo{RID: "a"}))
	r.FlushTo(second.Begin(RequestInfo{RID: "b"}))
	r.Step(relayStep("two", `{}`, `{}`))
	if _, steps, _, _ := first.snapshot(); steps != 2 {
		t.Errorf("first target has %d steps, want 2", steps)
	}
	if _, steps, _, _ := second.snapshot(); steps != 0 {
		t.Errorf("second target has %d steps, want 0", steps)
	}
}

// Past the ceiling a step reaches a real sink with its sizes and no bodies,
// and the reader reports it truncated. A smaller body still fits afterwards.
func TestRelayCeilingKeepsSizes(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(dir, ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	tr := sink.Begin(RequestInfo{RID: "CbRelayCeiling01", StartedAt: time.Now()})

	big := fmt.Sprintf(`{"body":"%s"}`, repeat('x', 200))
	r := NewRelay(100)
	r.FlushTo(tr)
	r.Step(relayStep("small", `{"a":1}`, `{"b":2}`))
	r.Step(relayStep("big", big, big))
	r.Step(relayStep("after", `{"c":3}`, `{"d":4}`))
	r.Close()
	tr.End("ok", "", nil)

	rd, err := OpenReader("file", StoreConfig{Dir: dir, Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	d, err := rd.Get(context.Background(), "CbRelayCeiling01", true)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Step{}
	for _, s := range d.Steps {
		byName[s.Name] = s
	}
	if s := byName["small"]; s.InputTruncated || s.In == nil {
		t.Errorf("a step under the ceiling lost its body: %+v", s)
	}
	s := byName["big"]
	if s.In != nil || s.Out != nil {
		t.Errorf("a step over the ceiling kept a body: %+v", s)
	}
	if !s.InputTruncated || !s.OutputTruncated || s.InputBytes != int64(len(big)) || s.OutputBytes != int64(len(big)) {
		t.Errorf("a dropped body lost its sizes: %+v", s)
	}
	if s := byName["after"]; s.In == nil {
		t.Errorf("a small body past a big one should still fit: %+v", s)
	}
}

// 0 keeps metadata only: this is the operator's lever.
func TestRelayCeilingZero(t *testing.T) {
	sink := &recordingSink{}
	tr := sink.Begin(RequestInfo{RID: "r"})
	r := NewRelay(0)
	r.FlushTo(tr)
	r.Step(relayStep("one", `{"a":1}`, `{"b":2}`))

	sink.mu.Lock()
	defer sink.mu.Unlock()
	s := sink.steps[0]
	if s.Input != nil || s.Output != nil || s.InputBytes != 7 || s.OutputBytes != 7 {
		t.Errorf("step = %+v", s)
	}
}

// The ceiling holds under concurrent steps, a held buffer flushes without
// loss, and nothing is forwarded after Close. Run with -race.
func TestRelayConcurrent(t *testing.T) {
	sink := &recordingSink{}
	tr := sink.Begin(RequestInfo{RID: "r"})
	r := NewRelay(1 << 30)

	var wg sync.WaitGroup
	const writers, each = 8, 200
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				r.Step(relayStep(fmt.Sprintf("%d-%d", i, j), `{}`, `{}`))
				r.Event(TimelineEvent{Event: "e"})
			}
		}(i)
	}
	r.FlushTo(tr)
	wg.Wait()
	r.Close()

	if _, steps, events, _ := sink.snapshot(); steps != writers*each || events != writers*each {
		t.Errorf("steps=%d events=%d, want %d of each", steps, events, writers*each)
	}
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

// Sanity for the reader above: a dropped body is not an empty object on disk.
func TestRelayDroppedBodyFilesAreEmpty(t *testing.T) {
	dir := t.TempDir()
	sink, err := NewFileSink(dir, ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	tr := sink.Begin(RequestInfo{RID: "CbRelayEmpty0001", StartedAt: time.Now()})
	r := NewRelay(0)
	r.FlushTo(tr)
	r.Step(relayStep("one", `{"a":1}`, `{"b":2}`))
	r.Close()
	tr.End("ok", "", nil)

	matches, err := filepath.Glob(filepath.Join(dir, "requests", "CbRelayEmpty0001", "steps", "*", "meta.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("meta files: %v %v", matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["input_truncated"] != true || m["output_truncated"] != true {
		t.Errorf("meta = %s", b)
	}
}
