package blevestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

func init() {
	search.Register("bleve", func(cfg search.Config) (search.Store, error) {
		return New(cfg.Path, cfg.MaxOpenIndexes)
	})
}

const (
	// defaultMaxOpen bounds the open indexes when the config gives no number.
	// An open index holds file handles and a few goroutines; a closed one costs
	// only disk. Reopening a thousand-record index and answering its first
	// query takes about 20 ms (BenchmarkReopenCollection).
	defaultMaxOpen = 64

	metaFile = "meta.json"
	indexDir = "index"

	// dropWait is how long DropCollection waits for readers to let go.
	dropWait = 5 * time.Second
)

// Store is the bundled lexical search backend. Each (tenant, collection) is its
// own Bleve index on local disk:
//
//	<root>/t_<tenant hash>/c_<collection hash>/meta.json
//	<root>/t_<tenant hash>/c_<collection hash>/index/
//
// One index per collection keeps ranking local to the collection, makes a drop
// a directory removal, and lets a collection be copied, moved or rebuilt alone.
// The cost is many small indexes, so only the recently used ones stay open.
//
// The directory names are hashes, never the tenant's or the author's strings,
// so no name can reach the filesystem. meta.json holds the real names.
type Store struct {
	root    string
	maxOpen int

	mu     sync.Mutex
	open   map[string]*entry
	clock  uint64
	closed bool

	// ensureMu serialises collection creation, which is rare and must not run
	// twice over one directory.
	ensureMu sync.Mutex
}

// entry is one collection's place in the open set. ready is closed once the
// open attempt finishes, so a second caller waits for the first caller's open
// and never starts its own.
type entry struct {
	ready chan struct{}
	col   *Collection
	err   error
	refs  int
	used  uint64
}

// meta is the sidecar beside each index. The index's own internal values stay
// the authority on the analyzer and scoring model (collection.go checks them
// on every open); this file exists so a listing never has to open an index.
type meta struct {
	Tenant          string `json:"tenant"`
	Name            string `json:"name"`
	AnalyzerVersion string `json:"analyzer_version"`
	ScoringModel    string `json:"scoring_model"`
	CreatedAt       string `json:"created_at"`
}

// New opens (creating if needed) the store rooted at dir.
func New(dir string, maxOpen int) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blevestore: a root directory is required (--search-path)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blevestore: create %s: %w", dir, err)
	}
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpen
	}
	return &Store{root: dir, maxOpen: maxOpen, open: map[string]*entry{}}, nil
}

// Hex only: a tenant or collection name never becomes a path element.
func hashOf(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

func (s *Store) tenantDir(tenant string) string { return filepath.Join(s.root, "t_"+hashOf(tenant)) }

func (s *Store) collectionDir(tenant, name string) string {
	return filepath.Join(s.tenantDir(tenant), "c_"+hashOf(tenant, name))
}

func readMeta(dir string) (meta, bool, error) {
	var m meta
	raw, err := os.ReadFile(filepath.Join(dir, metaFile))
	if errors.Is(err, os.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, false, fmt.Errorf("blevestore: %s: %w", filepath.Join(dir, metaFile), err)
	}
	return m, true, nil
}

// acquire returns the open collection and a release func. It opens the index
// when it is not in the open set, and may close the least recently used idle
// one to make room. Callers must call release exactly once.
func (s *Store) acquire(tenant, name string) (*Collection, func(), error) {
	dir := s.collectionDir(tenant, name)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, errors.New("blevestore: store is closed")
	}
	e, ok := s.open[dir]
	if ok {
		e.refs++
		s.mu.Unlock()
		<-e.ready
		if e.err != nil {
			s.release(dir, e)
			return nil, nil, e.err
		}
		return e.col, func() { s.release(dir, e) }, nil
	}
	e = &entry{ready: make(chan struct{}), refs: 1}
	s.open[dir] = e
	s.mu.Unlock()

	// Open outside the lock: other collections stay reachable meanwhile.
	if _, found, err := readMeta(dir); err != nil {
		e.err = err
	} else if !found {
		e.err = &search.CollectionNotFoundError{Tenant: tenant, Collection: name}
	} else {
		e.col, e.err = OpenCollection(filepath.Join(dir, indexDir))
		if me, ok := e.err.(*search.AnalyzerMismatchError); ok {
			me.Collection = name
		}
	}
	close(e.ready)
	if e.err != nil {
		s.release(dir, e)
		return nil, nil, e.err
	}
	return e.col, func() { s.release(dir, e) }, nil
}

func (s *Store) release(dir string, e *entry) {
	s.mu.Lock()
	e.refs--
	s.clock++
	e.used = s.clock
	var toClose []*Collection
	if e.err != nil && e.refs == 0 && s.open[dir] == e {
		delete(s.open, dir) // a failed open is never cached
	}
	for len(s.open) > s.maxOpen {
		var victimDir string
		var victim *entry
		for d, c := range s.open {
			if c.refs == 0 && c.col != nil && (victim == nil || c.used < victim.used) {
				victimDir, victim = d, c
			}
		}
		if victim == nil {
			break // everything open is in use; the bound is soft under load
		}
		delete(s.open, victimDir)
		toClose = append(toClose, victim.col)
	}
	s.mu.Unlock()
	for _, c := range toClose {
		_ = c.Close()
	}
}

// OpenCount reports how many indexes are open now.
func (s *Store) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.open)
}

func (s *Store) EnsureCollection(_ context.Context, tenant string, c search.Collection) error {
	if err := search.ValidateCollectionName(c.Name); err != nil {
		return err
	}
	for _, pin := range []struct{ field, got, want string }{
		{"analyzer_version", c.AnalyzerVersion, AnalyzerVersion},
		{"scoring_model", c.ScoringModel, ScoringModel},
	} {
		if pin.got != "" && pin.got != pin.want {
			return &search.AnalyzerMismatchError{Collection: c.Name, Field: pin.field, Existing: pin.want, Requested: pin.got}
		}
	}
	dir := s.collectionDir(tenant, c.Name)
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if _, found, err := readMeta(dir); err != nil || found {
		return err
	}
	// Create the index first and the sidecar last: a crash between the two
	// leaves a directory with no meta.json, which reads as "no collection" and
	// is simply created again over the same path.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("blevestore: create %s: %w", dir, err)
	}
	idx := filepath.Join(dir, indexDir)
	if err := os.RemoveAll(idx); err != nil {
		return err
	}
	col, err := OpenCollection(idx)
	if err != nil {
		return err
	}
	if err := col.Close(); err != nil {
		return err
	}
	raw, _ := json.Marshal(meta{
		Tenant: tenant, Name: c.Name,
		AnalyzerVersion: AnalyzerVersion, ScoringModel: ScoringModel,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	tmp := filepath.Join(dir, metaFile+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, metaFile))
}

func (s *Store) DescribeCollection(_ context.Context, tenant, name string) (search.Collection, bool, error) {
	col, release, err := s.acquire(tenant, name)
	var nf *search.CollectionNotFoundError
	if errors.As(err, &nf) {
		return search.Collection{}, false, nil
	}
	if err != nil {
		return search.Collection{}, false, err
	}
	defer release()
	n, err := col.Count()
	if err != nil {
		return search.Collection{}, false, err
	}
	return search.Collection{Name: name, AnalyzerVersion: AnalyzerVersion, ScoringModel: ScoringModel, Records: n}, true, nil
}

func (s *Store) apply(ctx context.Context, tenant, collection string, m search.Mutation) (int, error) {
	col, release, err := s.acquire(tenant, collection)
	if err != nil {
		return 0, err
	}
	defer release()
	return col.Apply(ctx, m, nil)
}

func (s *Store) Upsert(ctx context.Context, tenant, collection string, items []search.Item) (int, error) {
	if err := search.ValidateItems(items); err != nil {
		return 0, err // before the lookup: a bad request is bad whatever exists
	}
	return s.apply(ctx, tenant, collection, search.Mutation{Op: search.OpUpsert, Items: items})
}

func (s *Store) Delete(ctx context.Context, tenant, collection string, sel search.Selector) (int, error) {
	if err := search.ValidateSelector(sel); err != nil {
		return 0, err
	}
	return s.apply(ctx, tenant, collection, search.Mutation{Op: search.OpDelete, Select: sel})
}

func (s *Store) Update(ctx context.Context, tenant, collection string, filter search.Filter, ch search.Change) (int, error) {
	if err := search.ValidateUpdate(filter, ch); err != nil {
		return 0, err
	}
	return s.apply(ctx, tenant, collection, search.Mutation{Op: search.OpUpdate, Filter: filter, Change: ch})
}

func (s *Store) Query(ctx context.Context, tenant, collection, q string, limit int, filter search.Filter) ([]search.Hit, error) {
	if _, err := search.ValidateQuery(q, limit); err != nil {
		return nil, err
	}
	col, release, err := s.acquire(tenant, collection)
	if err != nil {
		return nil, err
	}
	defer release()
	return col.Query(ctx, q, limit, filter)
}

func (s *Store) ListCollections(ctx context.Context, tenant string) ([]search.Collection, error) {
	dirs, err := os.ReadDir(s.tenantDir(tenant))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []search.Collection
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		m, found, err := readMeta(filepath.Join(s.tenantDir(tenant), d.Name()))
		if err != nil {
			return nil, err
		}
		if !found || m.Tenant != tenant {
			continue
		}
		c, ok, err := s.DescribeCollection(ctx, tenant, m.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DropCollection(ctx context.Context, tenant, name string) (int, error) {
	c, found, err := s.DescribeCollection(ctx, tenant, name)
	if err != nil || !found {
		return 0, err
	}
	dir := s.collectionDir(tenant, name)
	// Take the collection out of the open set, waiting for readers to finish.
	deadline := time.Now().Add(dropWait)
	for {
		s.mu.Lock()
		e, open := s.open[dir]
		if !open || e.refs == 0 {
			delete(s.open, dir)
			s.mu.Unlock()
			if open && e.col != nil {
				_ = e.col.Close()
			}
			break
		}
		s.mu.Unlock()
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, fmt.Errorf("blevestore: collection %q is busy; not dropped", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The sidecar goes first: from here the collection reads as absent.
	if err := os.Remove(filepath.Join(dir, metaFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return 0, err
	}
	return int(c.Records), nil
}

// Shared reports false: this store is node-local disk.
func (s *Store) Shared() bool { return false }

// Close closes every open index. Calls after Close fail.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	open := s.open
	s.open = map[string]*entry{}
	s.mu.Unlock()
	var first error
	for _, e := range open {
		<-e.ready
		if e.col != nil {
			if err := e.col.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}
