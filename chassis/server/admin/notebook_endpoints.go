package admin

// Read-only operator surface over the notebook store (txco://notebook/*):
//
//	GET /v1/tenants/{tenant}/notebooks/{namespace}            ?prefix&after&limit
//	GET /v1/tenants/{tenant}/notebooks/{namespace}/{name}     ?after&since&until&tail&type&limit[&format=ndjson]
//
// Lists the notebooks a namespace holds and reads one back — the same
// selection the read op offers, always ascending by seq — as JSON, or as
// NDJSON when `format=ndjson` (or Accept: application/x-ndjson) so
// `txco notebook export` streams straight to a file. Cursors are the
// opaque strings the ops issue; a bare number is refused.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	chnotebook "github.com/loremlabs/thanks-computer/chassis/notebook"
)

// notebookHeadRow is one notebook on the wire.
type notebookHeadRow struct {
	Name      string `json:"name"`
	HighSeq   int64  `json:"high_seq"`
	TTLSecs   int64  `json:"ttl_secs"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type notebookListResponse struct {
	Namespace string            `json:"namespace"`
	Notebooks []notebookHeadRow `json:"notebooks"`
	Next      string            `json:"next,omitempty"`
	Count     int               `json:"count"`
}

// notebookEntryRow is one entry on the wire — and one NDJSON line.
type notebookEntryRow struct {
	Seq       int64           `json:"seq"`
	At        string          `json:"at"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	ObjectKey string          `json:"object_key,omitempty"`
}

type notebookReadResponse struct {
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Entries   []notebookEntryRow `json:"entries"`
	Next      string             `json:"next,omitempty"`
	Cursor    string             `json:"cursor,omitempty"`
	Count     int                `json:"count"`
	Truncated bool               `json:"truncated"`
}

// SetNotebookStore wires the notebook store the read-only endpoints use.
// Nil-safe: unset ⇒ the endpoints answer 503.
func (c *Controller) SetNotebookStore(st *chnotebook.Store) { c.notebookStore = st }

func notebookEntryRowOf(e chnotebook.Entry) notebookEntryRow {
	return notebookEntryRow{Seq: e.Seq, At: chnotebook.FormatAt(e.At), Type: e.Type, Data: e.Data, ObjectKey: e.ObjectKey}
}

// notebookPrelude is the common head: capability, tenant slug, store.
// Tenant is the SLUG (ac.TenantSlug): the ops compose notebook ids under
// the tenant slug (processor.TenantScope), NOT the numeric TenantID.
func (c *Controller) notebookPrelude(w http.ResponseWriter, r *http.Request) (slug, ns string, ok bool) {
	if err := policy.RequireCapability(r.Context(), "notebook:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return "", "", false
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_slug_missing", nil)
		return "", "", false
	}
	if c.notebookStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "notebook_store_unavailable",
			map[string]any{"hint": "no notebook store on this node (--notebook-store)"})
		return "", "", false
	}
	return ac.TenantSlug, mux.Vars(r)["namespace"], true
}

// writeNotebookError maps a store error to a status by its code: a
// caller's mistake (bad argument, bad name, stale cursor) is 400; anything
// else is the store's fault.
func writeNotebookError(w http.ResponseWriter, err error) {
	code := chnotebook.ErrorCode(err)
	status := http.StatusInternalServerError
	switch code {
	case "txco_notebook_invalid_arg", "txco_notebook_invalid_name", "txco_notebook_stale_cursor", "txco_notebook_too_large":
		status = http.StatusBadRequest
	case "":
		code = "notebook_err"
	}
	writeJSONError(w, status, code, map[string]any{"message": err.Error()})
}

func queryInt(q url.Values, key string) int {
	if v := q.Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// notebookReadReqFromQuery lifts the read selection out of the query
// string, exactly as the op lifts it out of WITH.
func notebookReadReqFromQuery(q url.Values, ref chnotebook.Ref) (chnotebook.ReadReq, error) {
	req := chnotebook.ReadReq{Ref: ref}
	if a := q.Get("after"); a != "" {
		c, err := chnotebook.ParseCursor(a)
		if err != nil {
			return req, err
		}
		req.After = c
	}
	if v := q.Get("since"); v != "" {
		t, err := chnotebook.ParseAt(v)
		if err != nil {
			return req, &chnotebook.InvalidArgError{Reason: "since must be an RFC 3339 timestamp"}
		}
		req.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := chnotebook.ParseAt(v)
		if err != nil {
			return req, &chnotebook.InvalidArgError{Reason: "until must be an RFC 3339 timestamp"}
		}
		req.Until = t
	}
	req.Tail = queryInt(q, "tail")
	req.Type = q.Get("type")
	req.Limit = queryInt(q, "limit")
	return req, nil
}

// handleListNotebooks lists the URL tenant's notebooks under {namespace},
// optionally by name prefix, windowed by ?limit and ?after (a name).
func (c *Controller) handleListNotebooks(w http.ResponseWriter, r *http.Request) {
	slug, ns, ok := c.notebookPrelude(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	res, err := c.notebookStore.List(r.Context(), slug, ns, q.Get("prefix"), q.Get("after"), queryInt(q, "limit"))
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	rows := make([]notebookHeadRow, 0, len(res.Notebooks))
	for _, h := range res.Notebooks {
		rows = append(rows, notebookHeadRow{Name: h.Name, HighSeq: h.HighSeq, TTLSecs: int64(h.TTL.Seconds()),
			CreatedAt: chnotebook.FormatAt(h.CreatedAt), UpdatedAt: chnotebook.FormatAt(h.UpdatedAt)})
	}
	writeJSON(w, http.StatusOK, notebookListResponse{Namespace: ns, Notebooks: rows, Next: res.Next, Count: len(rows)})
}

// handleReadNotebook reads {name} under {namespace}: one JSON page, or —
// with ?format=ndjson / Accept: application/x-ndjson — every selected
// entry streamed one object per line. The stream's status is committed
// on the first line, so a selection error surfaces as 400 only when
// nothing has been written yet.
func (c *Controller) handleReadNotebook(w http.ResponseWriter, r *http.Request) {
	slug, ns, ok := c.notebookPrelude(w, r)
	if !ok {
		return
	}
	name := mux.Vars(r)["name"]
	ref := chnotebook.Ref{Tenant: slug, Namespace: ns, Name: name}
	q := r.URL.Query()
	req, err := notebookReadReqFromQuery(q, ref)
	if err != nil {
		writeNotebookError(w, err)
		return
	}
	ndjson := q.Get("format") == "ndjson" || strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
	if !ndjson {
		res, err := c.notebookStore.Read(r.Context(), req)
		if err != nil {
			writeNotebookError(w, err)
			return
		}
		rows := make([]notebookEntryRow, 0, len(res.Entries))
		for _, e := range res.Entries {
			rows = append(rows, notebookEntryRowOf(e))
		}
		writeJSON(w, http.StatusOK, notebookReadResponse{Namespace: ns, Name: name, Entries: rows,
			Next: res.Next.Encode(), Cursor: res.Cursor.Encode(), Count: len(rows), Truncated: res.Truncated})
		return
	}

	started := false
	enc := json.NewEncoder(w) // Encode appends the newline: one object per line
	enc.SetEscapeHTML(false)
	rowLimit := req.Limit
	req.Limit = 0
	count := 0
	err = c.notebookStore.ForEach(r.Context(), req, func(e chnotebook.Entry) error {
		if rowLimit > 0 && count >= rowLimit {
			return errNotebookStreamStop
		}
		if !started {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		count++
		return enc.Encode(notebookEntryRowOf(e))
	})
	if err != nil && err != errNotebookStreamStop {
		if !started {
			writeNotebookError(w, err)
		}
		return
	}
	if !started {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

var errNotebookStreamStop = &notebookStreamStop{}

type notebookStreamStop struct{}

func (*notebookStreamStop) Error() string { return "notebook stream: row limit reached" }
