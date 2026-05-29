package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/artifact/filestore"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

func withAStore(t *testing.T, c *Controller) {
	t.Helper()
	s, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	c.SetArtifactStore(s)
}

func activate(t *testing.T, c *Controller, stack string, n int64) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/"+stack+"/activate",
		mustJSON(t, activateRequest{VersionNumber: n}),
		map[string]string{"name": stack}), testTenant)
	c.handleActivateStack(w, r)
	return w
}

func computeRef(wasm []byte) compute.Ref {
	sum := sha256.Sum256(wasm)
	return compute.Ref{Alg: "sha256", Digest: hex.EncodeToString(sum[:])}
}

// Activate must reject a rule referencing a compute artifact that isn't in the
// store — else it would materialise and fail at runtime with "not found".
func TestActivateRejectsMissingComputeArtifact(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	withAStore(t, c)
	ref := computeRef([]byte("nonexistent module bytes"))

	v := callCreateDraft(t, c, "cstack", "")
	callPutFiles(t, c, "cstack", v, []stackFile{
		{Path: "100/x.txcl", Content: `EXEC "` + ref.String() + `"`},
	})

	w := activate(t, c, "cstack", v)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("activate code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "missing_compute_artifact") {
		t.Fatalf("body missing missing_compute_artifact: %s", w.Body.String())
	}
	if n := opsCount(t, c, "cstack"); n != 0 {
		t.Fatalf("ops materialised despite rejected activate: %d", n)
	}
}

// Activate succeeds once the referenced artifact is present in the store.
func TestActivateAcceptsPresentComputeArtifact(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	withAStore(t, c)
	wasm := []byte("a present module")
	ref := computeRef(wasm)
	if err := c.astore.Put(c.ctx, ref.StoreRef(), wasm, []byte(`{"engine":"wazero"}`)); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}

	v := callCreateDraft(t, c, "cstack", "")
	callPutFiles(t, c, "cstack", v, []stackFile{
		{Path: "100/x.txcl", Content: `EXEC "` + ref.String() + `"`},
	})

	w := activate(t, c, "cstack", v)
	if w.Code != http.StatusOK {
		t.Fatalf("activate code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if n := opsCount(t, c, "cstack"); n != 1 {
		t.Fatalf("ops count = %d, want 1", n)
	}
}

func TestPutComputeStoresAndHeadFinds(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	withAStore(t, c)
	wasm := []byte("\x00asm fake module bytes")
	ref := computeRef(wasm)

	// PUT
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPut,
		"/v1/tenants/default/computes/sha256/"+ref.Digest, wasm,
		map[string]string{"alg": "sha256", "digest": ref.Digest}), testTenant)
	c.handlePutCompute(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT code = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// HEAD finds it
	hw := httptest.NewRecorder()
	hr := withTenantAdminCtx(muxRequest(http.MethodHead,
		"/v1/tenants/default/computes/sha256/"+ref.Digest, nil,
		map[string]string{"alg": "sha256", "digest": ref.Digest}), testTenant)
	c.handleHeadCompute(hw, hr)
	if hw.Code != http.StatusOK {
		t.Fatalf("HEAD code = %d, want 200", hw.Code)
	}
}

func TestPutComputeRejectsDigestMismatch(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	withAStore(t, c)
	wasm := []byte("some bytes")

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPut,
		"/v1/tenants/default/computes/sha256/deadbeef", wasm,
		map[string]string{"alg": "sha256", "digest": "deadbeef"}), testTenant)
	c.handlePutCompute(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PUT mismatch code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "digest_mismatch") {
		t.Fatalf("body missing digest_mismatch: %s", w.Body.String())
	}
}
