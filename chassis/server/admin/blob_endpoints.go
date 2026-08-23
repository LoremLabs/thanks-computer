package admin

// Read-only operator surface over the blob name index
// (GET /v1/tenants/{tenant}/stacks/{name}/blobs): the names a stack's BLOBS/
// tree seeded, with the drift flag `txco data apply` consults before pushing
// (a seeded name the runtime repointed since the tree last shipped it) and
// `txco data pull` uses to materialise the live content back into the tree.

import (
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/blob"
)

// stackBlobRow is one seeded name on the wire.
type stackBlobRow struct {
	Name        string `json:"name"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
	SeededSHA   string `json:"seeded_sha"`
	Drifted     bool   `json:"drifted"`
	UpdatedAt   string `json:"updated_at"`
}

type stackBlobsResponse struct {
	Stack string         `json:"stack"`
	Names []stackBlobRow `json:"names"`
	Count int            `json:"count"`
}

// SetBlobIndex wires the blob name index the stack-blobs endpoint reads.
// Nil-safe: unset ⇒ the endpoint answers 503.
func (c *Controller) SetBlobIndex(ix blob.Index) { c.blobIndex = ix }

// handleListStackBlobs lists the names seeded by {name}'s BLOBS/ tree (rows
// whose seeded_by is the stack), with their live sha, the sha the tree last
// shipped, and whether they drifted. Tenant is the SLUG (ac.TenantSlug): the
// index is keyed the way the runtime reads it (processor.TenantScope).
func (c *Controller) handleListStackBlobs(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_slug_missing", nil)
		return
	}
	if c.blobIndex == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "blob_index_unavailable",
			map[string]any{"hint": "no blob index on this node"})
		return
	}
	stack := mux.Vars(r)["name"]
	page, err := c.blobIndex.ListNames(r.Context(), ac.TenantSlug, blob.ListOpts{})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "blob_index_err", map[string]any{"err": err.Error()})
		return
	}
	rows := make([]stackBlobRow, 0)
	for _, n := range page.Names {
		if n.SeededBy != stack {
			continue
		}
		rows = append(rows, stackBlobRow{
			Name: n.Name, SHA256: n.SHA256, Size: n.Size, ContentType: n.ContentType,
			Filename: n.Filename, SeededSHA: n.SeededSHA, Drifted: n.Drifted(),
			UpdatedAt: n.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, stackBlobsResponse{Stack: stack, Names: rows, Count: len(rows)})
}
