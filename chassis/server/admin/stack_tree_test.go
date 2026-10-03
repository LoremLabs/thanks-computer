package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/stackdir"
)

func TestValidateStackFilePathStackDir(t *testing.T) {
	if err := validateStackFilePath(stackdir.Path(srcDigest)); err != nil {
		t.Fatalf("valid STACKDIR/ row refused: %v", err)
	}
	for _, bad := range []string{
		"STACKDIR/" + srcDigest + ".json",
		"STACKDIR/abc.tar",
		"STACKDIR/x/" + srcDigest + ".tar",
		"stackdir/" + srcDigest + ".tar",
	} {
		if err := validateStackFilePath(bad); err == nil {
			t.Errorf("validateStackFilePath(%q) accepted", bad)
		}
	}
}

// A STACKDIR/ row is fingerprint-only, names its own hash, and a version
// carries one; a resident bundle is stored hash-only, skipped by activation's
// op materialisation and shipped to the fleet as its fingerprint.
func TestPutDraftFilesStackTreeRow(t *testing.T) {
	ctx := context.Background()
	c, _, _ := computeSourceFixture(t)
	data, hash, err := stackdir.Pack([]stackdir.File{{Path: "2100_SETUP/race.py", Content: []byte("print(1)\n")}})
	if err != nil {
		t.Fatal(err)
	}
	other, otherHash, _ := stackdir.Pack([]stackdir.File{{Path: "x", Content: []byte("2")}})
	path := stackdir.Path(hash)

	if rec := putDraftFiles(c, []map[string]string{{"path": path, "content": string(data)}}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "cas_required") {
		t.Fatalf("inline STACKDIR/: %d %s", rec.Code, rec.Body.String())
	}
	if err := c.fcas.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	if err := c.fcas.Put(ctx, otherHash, other); err != nil {
		t.Fatal(err)
	}
	// The path names one bundle, the hash another.
	if rec := putDraftFiles(c, []map[string]string{{"path": path, "content_hash": otherHash, "encoding": "cas"}}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "hash_mismatch") {
		t.Fatalf("mismatched STACKDIR/: %d %s", rec.Code, rec.Body.String())
	}
	// Two trees in one version.
	two := []map[string]string{
		{"path": path, "content_hash": hash, "encoding": "cas"},
		{"path": stackdir.Path(otherHash), "content_hash": otherHash, "encoding": "cas"},
	}
	if rec := putDraftFiles(c, two); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "duplicate_stack_tree") {
		t.Fatalf("two STACKDIR/ rows: %d %s", rec.Code, rec.Body.String())
	}
	row := map[string]string{"path": path, "content_hash": hash, "encoding": "cas"}
	if rec := putDraftFiles(c, []map[string]string{row}); rec.Code != http.StatusOK {
		t.Fatalf("resident bundle: %d %s", rec.Code, rec.Body.String())
	}
	var content, stored string
	if err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT content, content_hash FROM stack_files WHERE version_id=1 AND path=?`, path).Scan(&content, &stored); err != nil {
		t.Fatal(err)
	}
	if content != "" || stored != hash {
		t.Fatalf("stored row content=%q hash=%q", content, stored)
	}
	for mode, enc := range map[contentMode]string{contentOps: "", contentAll: "cas"} {
		files, err := c.loadVersionFiles(ctx, 1, mode)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Encoding != enc || files[0].Content != "" || files[0].ContentHash != hash {
			t.Fatalf("mode %d row = %+v", mode, files)
		}
	}
	art, err := c.readStackFilesForArtifact(ctx, "tnt_default", "site", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 1 || art[0].Path != path || art[0].ContentHash != hash || art[0].Content != "" {
		t.Fatalf("fleet artifact files = %+v", art)
	}
}
