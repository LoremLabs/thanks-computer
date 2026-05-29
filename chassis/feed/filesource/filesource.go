// Package filesource is the local-directory feed source: zero
// infrastructure, directly inspectable, useful for development and as a
// drop point a sidecar can write into.
//
// Each event is one JSON file in the directory (any name ending .json),
// holding a single controlevent.Event. Poll returns the events with
// ControlVersion > since, sorted ascending. A malformed file fails the
// poll loudly (the applier logs and retries next tick) rather than
// silently dropping an event and letting the fleet diverge.
package filesource

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/feed"
)

func init() {
	feed.Register("file", func(cfg feed.SourceConfig) (feed.Source, error) {
		return New(cfg.FileDir)
	})
}

// FileSource reads events from a directory.
type FileSource struct {
	dir string
}

// New returns a FileSource rooted at dir, creating dir (with parents) if
// absent. A failure here is a startup error.
func New(dir string) (*FileSource, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileSource{dir: dir}, nil
}

func (*FileSource) Name() string { return "file" }

func (fs *FileSource) Poll(_ context.Context, since uint64) ([]controlevent.Event, error) {
	ents, err := os.ReadDir(fs.dir)
	if err != nil {
		return nil, err
	}
	var out []controlevent.Event
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(fs.dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var ev controlevent.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			return nil, fmt.Errorf("feed/filesource: %s: %w", e.Name(), err)
		}
		if ev.ControlVersion > since {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ControlVersion < out[j].ControlVersion
	})
	return out, nil
}

var _ feed.Source = (*FileSource)(nil)
