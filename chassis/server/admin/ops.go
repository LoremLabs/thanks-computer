package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// OpRecord is the wire shape for a single rule in the admin API. It maps 1:1
// to a row in the `ops` table.
//
// Identity is `(stack, scope, name)` — the name comes from the rule's
// filename minus the `.txcl` extension on the developer's disk. Multiple
// rules per (stack, scope) are first-class: each name is one row, and each
// runs in parallel at that stage.
type OpRecord struct {
	Stack   string `json:"stack"`
	Scope   int    `json:"scope"`
	Name    string `json:"name"`
	Txcl    string `json:"txcl"`
	MockReq string `json:"mock_req,omitempty"`
	MockRes string `json:"mock_res,omitempty"`
}

type listOpsResponse struct {
	Ops []OpRecord `json:"ops"`
}

type errorResponse struct {
	Error  string         `json:"error"`
	Detail map[string]any `json:"detail,omitempty"`
}

// handleListOps lists all rules in the `ops` table, optionally filtered by
// `?stack=<prefix>` (matches the exact stack and any descendants under it).
func (c *Controller) handleListOps(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	prefix := r.URL.Query().Get("stack")

	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = c.pu.RuntimeDB.QueryContext(r.Context(),
			`SELECT stack, scope, name, txcl, mock_req, mock_res FROM ops ORDER BY stack, scope, name, txcl`)
	} else {
		rows, err = c.pu.RuntimeDB.QueryContext(r.Context(),
			`SELECT stack, scope, name, txcl, mock_req, mock_res FROM ops
			 WHERE stack = ? OR stack LIKE ?
			 ORDER BY stack, scope, name, txcl`,
			prefix, prefix+"/%")
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query failed", map[string]any{"err": err.Error()})
		return
	}
	defer rows.Close()

	var ops []OpRecord
	for rows.Next() {
		var rec OpRecord
		if err := rows.Scan(&rec.Stack, &rec.Scope, &rec.Name, &rec.Txcl, &rec.MockReq, &rec.MockRes); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "scan failed", map[string]any{"err": err.Error()})
			return
		}
		ops = append(ops, rec)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "rows iteration", map[string]any{"err": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, listOpsResponse{Ops: ops})
}

// handleImportOps was the legacy flat bulk-upsert into the `ops`
// table. It is intentionally removed — the versioned control plane
// (POST /stacks/<name>/draft → PUT /versions/{n}/files → POST
// /stacks/<name>/activate) is the only supported write path. The
// retired routes registered in `server.go` return 410 for callers
// still hitting the old URL.

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeJSONError(w http.ResponseWriter, status int, msg string, detail map[string]any) {
	writeJSON(w, status, errorResponse{Error: msg, Detail: detail})
}

// stackPrefix returns the LIKE pattern that matches `prefix` and all
// descendants. Unused right now (handleListOps inlines it) but kept here for
// the future `apply --prune` path.
//
//nolint:unused // reserved for future prune flow
func stackPrefix(prefix string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix + "/%"
}
