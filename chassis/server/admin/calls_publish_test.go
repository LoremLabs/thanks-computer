package admin

import (
	"context"
	"testing"
)

// A CALLS/ declaration reaches the fleet as a CAPS/ one does: its bytes are
// put in the shared CAS and the artifact carries only the fingerprint. A
// fleet node's dbcache snapshot (the Postgres mirror) strips every
// stack_files row's content, so the call-entry source resolves the
// declaration from the CAS by its hash; shipped inline, it never got there
// and every `WITH entry` call failed "filecas: hash not found".
func TestFleetArtifactPutsCallDeclarationInTheCAS(t *testing.T) {
	ctx := context.Background()
	c, _, _ := computeSourceFixture(t)
	const path = "CALLS/mail_out.yaml"
	body := "description: Send one mail.\nentry: 7390\n"
	hash := sha256Hex(body)
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (1, ?, ?, ?)`,
		path, []byte(body), hash); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.fcas.Exists(ctx, hash); ok {
		t.Fatal("the declaration is in the CAS before the publish")
	}

	art, err := c.readStackFilesForArtifact(ctx, "tnt_default", "site", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 1 || art[0].Path != path || art[0].Content != "" || art[0].ContentHash != hash {
		t.Fatalf("fleet artifact files = %+v, want %s as its fingerprint only", art, path)
	}
	got, err := c.fcas.Get(ctx, hash)
	if err != nil {
		t.Fatalf("the declaration is not in the CAS after the publish: %v", err)
	}
	if string(got) != body {
		t.Fatalf("CAS bytes = %q, want %q", got, body)
	}
}
