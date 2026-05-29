// FileSink is the producer-side companion to FileSource: it writes
// one JSON file per event into the same directory the Source reads
// from. Useful for local multi-chassis testing without a broker, and
// as a reference implementation of the Sink contract.
//
// Conventions:
//   - Filename is "<event_id>.json". Same EventID on a retry overwrites
//     the same file — the natural backend-level dedup primitive (the
//     file Sink's analogue of JetStream's Nats-Msg-Id).
//   - ControlVersion is assigned in-process from a monotonic counter
//     primed by scanning the dir on first Append. Single-process safe;
//     multi-process is undefined (same as the rest of the file
//     backend — it's a dev convenience, not a clustered store).
//   - Re-publish of an existing event_id returns the SAME
//     ControlVersion the file already records, so retries don't
//     burn fresh sequence numbers.

package filesource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

func init() {
	feed.RegisterSink("file", func(cfg feed.SourceConfig) (feed.Sink, error) {
		return NewSink(cfg.FileDir)
	})
}

// FileSink writes events to a directory.
type FileSink struct {
	dir string

	mu      sync.Mutex
	counter uint64 // highest assigned ControlVersion; lazily primed on first Append
	primed  bool
}

// NewSink returns a FileSink rooted at dir, creating dir if absent.
func NewSink(dir string) (*FileSink, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileSink{dir: dir}, nil
}

func (*FileSink) Name() string { return "file" }

// Append writes e to <dir>/<event_id>.json with a stamped
// ControlVersion. Idempotent on EventID: a retry returns the same
// ControlVersion the existing file records.
func (s *FileSink) Append(_ context.Context, e controlevent.Event) (controlevent.Event, error) {
	if e.EventID == "" {
		return e, fmt.Errorf("feed/filesource: Append requires non-empty EventID")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.primed {
		max, err := s.scanMaxCV()
		if err != nil {
			return e, fmt.Errorf("feed/filesource: prime counter: %w", err)
		}
		s.counter = max
		s.primed = true
	}

	// Idempotent re-publish: same event_id ⇒ same control_version.
	path := filepath.Join(s.dir, safeFilename(e.EventID)+".json")
	if existing, ok, err := readExisting(path); err != nil {
		return e, err
	} else if ok {
		// Trust the file's ControlVersion; ignore any drift in other
		// fields (the producer of record is whoever wrote first).
		e.ControlVersion = existing.ControlVersion
		return e, nil
	}

	s.counter++
	e.ControlVersion = s.counter

	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		s.counter-- // give back the unused slot
		return e, fmt.Errorf("feed/filesource: marshal: %w", err)
	}
	if err := writeAtomic(path, b); err != nil {
		s.counter--
		return e, fmt.Errorf("feed/filesource: write %s: %w", path, err)
	}
	return e, nil
}

// scanMaxCV reads every .json file in dir and returns the highest
// ControlVersion. Used to prime the in-memory counter on first
// Append so we don't reuse sequence numbers after a restart.
func (s *FileSink) scanMaxCV() (uint64, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	var max uint64
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ev, ok, err := readExisting(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return 0, err
		}
		if ok && ev.ControlVersion > max {
			max = ev.ControlVersion
		}
	}
	return max, nil
}

func readExisting(path string) (controlevent.Event, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return controlevent.Event{}, false, nil
		}
		return controlevent.Event{}, false, err
	}
	var ev controlevent.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		return ev, false, fmt.Errorf("feed/filesource: parse %s: %w", filepath.Base(path), err)
	}
	return ev, true, nil
}

// writeAtomic writes to a tmp file then renames. Atomic on POSIX
// against a concurrent reader holding the old file.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// safeFilename strips path separators so an attacker-controlled
// EventID can't escape the dir. We don't expect EventIDs to contain
// these, but defense in depth.
func safeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	return s
}

var _ feed.Sink = (*FileSink)(nil)
