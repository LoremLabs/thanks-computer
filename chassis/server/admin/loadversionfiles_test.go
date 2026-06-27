package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// mapStore is a filecas.Store double whose Get returns canned bytes (countingStore.Get
// returns nil, which won't exercise the read-back path).
type mapStore struct{ m map[string][]byte }

func (s *mapStore) Get(_ context.Context, hash string) ([]byte, error) {
	b, ok := s.m[hash]
	if !ok {
		return nil, filecas.ErrNotFound
	}
	return b, nil
}
func (s *mapStore) Put(context.Context, string, []byte) error { return nil }
func (s *mapStore) Exists(_ context.Context, hash string) (bool, error) {
	_, ok := s.m[hash]
	return ok, nil
}
func (s *mapStore) Name() string { return "map" }

// loadVersionFiles must resolve a fingerprint-only (materialised) row's bytes from the
// filecas — the `txco pull` 0-byte fix — and base64-encode binary so json.Marshal can't
// rewrite it to U+FFFD. Inline text passes through raw; a genuinely-empty file stays
// empty and never touches the CAS.
func TestLoadVersionFilesResolvesCASAndEncodesBinary(t *testing.T) {
	c := newTestController(t, config.Config{})
	bin := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0xC0} // invalid UTF-8
	binHash := sha256Hex(string(bin))
	c.fcas = &mapStore{m: map[string][]byte{binHash: bin}} // note: emptyHash NOT present

	const vid = 4242
	ins := func(path, content, hash string) {
		if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?,?,?,?)`,
			vid, path, content, hash); err != nil {
			t.Fatalf("insert %s: %v", path, err)
		}
	}
	ins("FILES/cover.jpg", "", binHash)             // fingerprint-only binary → resolve from CAS
	ins("FILES/a.txt", "hello", sha256Hex("hello")) // inline text → raw
	ins("FILES/empty", "", sha256Hex(""))           // genuinely empty → stays empty, NO CAS

	got, err := c.loadVersionFiles(context.Background(), vid, contentAll)
	if err != nil {
		t.Fatalf("loadVersionFiles: %v", err)
	}
	by := map[string]stackFile{}
	for _, f := range got {
		by[f.Path] = f
	}

	cov := by["FILES/cover.jpg"]
	if cov.Encoding != "base64" {
		t.Errorf("cover Encoding = %q, want base64", cov.Encoding)
	}
	if dec, derr := base64.StdEncoding.DecodeString(cov.Content); derr != nil || !bytes.Equal(dec, bin) {
		t.Errorf("cover round-trip: err=%v equal=%v", derr, bytes.Equal(dec, bin))
	}
	if txt := by["FILES/a.txt"]; txt.Encoding != "" || txt.Content != "hello" {
		t.Errorf("text = %q/%q, want hello/raw", txt.Content, txt.Encoding)
	}
	// The empty file must NOT consult the CAS — its hash isn't in mapStore, so a CAS
	// hit would ErrNotFound and fail loadVersionFiles. Reaching here = it was skipped.
	if e := by["FILES/empty"]; e.Content != "" || e.Encoding != "" {
		t.Errorf("empty = %q/%q, want empty/raw", e.Content, e.Encoding)
	}
}

// contentOps resolves op/mock bodies (non-FILES/ paths) from the CAS but must
// NOT touch FILES/ asset bytes — that asset fan-out is exactly the full-book R2
// download the ops view was needlessly triggering per stack. The asset hash is
// absent from mapStore, so any CAS read for it would ErrNotFound and fail the
// call; reaching the assertions proves the asset was skipped.
func TestLoadVersionFilesContentOpsSkipsAssets(t *testing.T) {
	c := newTestController(t, config.Config{})
	const txcl = "WHEN @x EMIT .y=1"
	txclHash := sha256Hex(txcl)
	// Only the op source is in the CAS; the asset's hash is deliberately absent.
	c.fcas = &mapStore{m: map[string][]byte{txclHash: []byte(txcl)}}

	const vid = 4343
	ins := func(path, content, hash string) {
		if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?,?,?,?)`,
			vid, path, content, hash); err != nil {
			t.Fatalf("insert %s: %v", path, err)
		}
	}
	ins("0100_QUERY/query.txcl", "", txclHash)                        // op source, stripped → resolve from CAS
	ins("FILES/_data/seq-692.md", "", sha256Hex("a-whole-book-here")) // asset, stripped → must be SKIPPED

	got, err := c.loadVersionFiles(context.Background(), vid, contentOps)
	if err != nil {
		t.Fatalf("loadVersionFiles(contentOps): %v", err)
	}
	by := map[string]stackFile{}
	for _, f := range got {
		by[f.Path] = f
	}
	if op := by["0100_QUERY/query.txcl"]; op.Content != txcl {
		t.Errorf("op txcl = %q, want %q (should resolve from CAS)", op.Content, txcl)
	}
	// Asset listed (path + hash) but body omitted; CAS never consulted.
	asset, ok := by["FILES/_data/seq-692.md"]
	if !ok {
		t.Fatalf("asset row missing from listing")
	}
	if asset.Content != "" {
		t.Errorf("asset content = %q, want empty (FILES/ skipped under contentOps)", asset.Content)
	}
	if asset.ContentHash == "" {
		t.Errorf("asset content_hash dropped; want it listed for diff/etag")
	}
}
