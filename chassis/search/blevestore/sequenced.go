package blevestore

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/blevesearch/bleve/v2"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// keyAppliedSeq holds the collection's position in its mutation log, inside
// the index, so a copy of the index carries its own cursor.
const keyAppliedSeq = "txco.applied_seq"

// engine names what wrote a snapshot. A reader that does not know the engine
// must not open it.
const engine = "bleve/v2 scorch"

func encodeSeq(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

func (c *Collection) appliedSeq() (uint64, error) {
	raw, err := c.Internal(keyAppliedSeq)
	if err != nil || len(raw) != 8 {
		return 0, err
	}
	return binary.BigEndian.Uint64(raw), nil
}

// ApplyAt implements search.Sequenced.
func (s *Store) ApplyAt(ctx context.Context, tenant, collection string, seq uint64, m search.Mutation) (int, bool, error) {
	if m.Op == search.OpDrop {
		n, err := s.DropCollection(ctx, tenant, collection)
		return n, err == nil, err
	}
	if m.Op == search.OpEnsure {
		c := m.Collection
		c.Name = collection
		if err := s.EnsureCollection(ctx, tenant, c); err != nil {
			return 0, false, err
		}
	}
	col, release, err := s.acquire(tenant, collection)
	if err != nil {
		return 0, false, err
	}
	defer release()
	have, err := col.appliedSeq()
	if err != nil {
		return 0, false, err
	}
	if seq <= have {
		return 0, false, nil
	}
	cursor := map[string][]byte{keyAppliedSeq: encodeSeq(seq)}
	if m.Op == search.OpEnsure {
		return 0, true, col.commit(col.idx.NewBatch(), cursor)
	}
	n, err := col.Apply(ctx, m, cursor)
	return n, err == nil, err
}

// AppliedSeq implements search.Sequenced.
func (s *Store) AppliedSeq(_ context.Context, tenant, collection string) (uint64, bool, error) {
	col, release, err := s.acquire(tenant, collection)
	var nf *search.CollectionNotFoundError
	if errors.As(err, &nf) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer release()
	seq, err := col.appliedSeq()
	return seq, err == nil, err
}

// StagingDir implements search.Snapshotter.
func (s *Store) StagingDir() (string, error) {
	dir := filepath.Join(s.root, ".staging")
	return dir, os.MkdirAll(dir, 0o755)
}

// Snapshot implements search.Snapshotter. Bleve's CopyTo copies from a
// reference-counted snapshot of the index, so the persister and the merger
// cannot remove a segment file while it is being read. Files of a live index
// are never read any other way.
func (s *Store) Snapshot(ctx context.Context, tenant, collection, dir string) (search.Manifest, error) {
	var man search.Manifest
	if _, err := os.Stat(dir); err == nil {
		return man, fmt.Errorf("blevestore: snapshot directory %s already exists", dir)
	}
	col, release, err := s.acquire(tenant, collection)
	if err != nil {
		return man, err
	}
	copyable, ok := col.idx.(bleve.IndexCopyable)
	if !ok {
		release()
		return man, errors.New("blevestore: this index type cannot be copied while open")
	}
	err = copyable.CopyTo(bleve.FileSystemDirectory(filepath.Join(dir, indexDir)))
	release()
	if err != nil {
		_ = os.RemoveAll(dir)
		return man, fmt.Errorf("blevestore: copy %q: %w", collection, err)
	}
	// Open the copy: it must stand on its own, and what it reports is what
	// the manifest says. The source may have moved on since the copy began.
	man, err = describeSnapshot(dir, tenant, collection)
	if err != nil {
		_ = os.RemoveAll(dir)
		return man, err
	}
	raw, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, search.ManifestFile), raw, 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return man, err
	}
	return man, nil
}

func describeSnapshot(dir, tenant, collection string) (search.Manifest, error) {
	// OpenCollection creates what it does not find. A snapshot must exist.
	if fi, err := os.Stat(filepath.Join(dir, indexDir)); err != nil || !fi.IsDir() {
		return search.Manifest{}, fmt.Errorf("blevestore: no snapshot of %q at %s", collection, dir)
	}
	cp, err := OpenCollection(filepath.Join(dir, indexDir))
	if err != nil {
		return search.Manifest{}, fmt.Errorf("blevestore: snapshot of %q does not open: %w", collection, err)
	}
	defer cp.Close()
	seq, err := cp.appliedSeq()
	if err != nil {
		return search.Manifest{}, err
	}
	n, err := cp.Count()
	if err != nil {
		return search.Manifest{}, err
	}
	return search.Manifest{
		Tenant: tenant, Collection: collection, AppliedSeq: seq, Records: n,
		AnalyzerVersion: AnalyzerVersion, ScoringModel: ScoringModel, Engine: engine,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// Install implements search.Snapshotter.
func (s *Store) Install(ctx context.Context, tenant, collection, dir string) (search.Manifest, error) {
	if err := search.ValidateCollectionName(collection); err != nil {
		return search.Manifest{}, err
	}
	// Check the snapshot before anything it would replace is touched. The
	// manifest it came with is advice; what the index reports is the truth.
	man, err := describeSnapshot(dir, tenant, collection)
	if err != nil {
		return man, err
	}
	s.ensureMu.Lock()
	defer s.ensureMu.Unlock()
	if _, err := s.DropCollection(ctx, tenant, collection); err != nil {
		return man, err
	}
	dst := s.collectionDir(tenant, collection)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return man, err
	}
	if err := os.Rename(filepath.Join(dir, indexDir), filepath.Join(dst, indexDir)); err != nil {
		return man, fmt.Errorf("blevestore: install %q: %w (is the snapshot under StagingDir?)", collection, err)
	}
	_ = os.RemoveAll(dir)
	if err := writeMeta(dst, tenant, collection); err != nil {
		return man, err
	}
	return man, nil
}
