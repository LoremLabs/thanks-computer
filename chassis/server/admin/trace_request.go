package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// traceTenantScope authorizes a trace request and returns the tenant slug to
// scope reads to. On a tenant-scoped route (/v1/tenants/{slug}/traces/…,
// where resolveTenantMiddleware has set ac.TenantSlug) it requires the
// opstack:*:trace capability and returns that slug — a tenant-owner (who
// holds opstack:*:*) passes, a non-member (empty caps) is denied. On a flat
// route (ac.TenantSlug == "") it requires super-admin and returns "" (no
// filter — the chassis-wide operator view, incl. _sys). ok=false means a
// 403 was already written.
func (c *Controller) traceTenantScope(w http.ResponseWriter, r *http.Request) (tenant string, ok bool) {
	ac := auth.FromContext(r.Context())
	if ac == nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return "", false
	}
	if ac.TenantSlug != "" {
		if err := policy.RequireCapability(r.Context(), "opstack:*:trace"); err != nil {
			auth.WriteForbidden(w, signature.ErrCapabilityDenied)
			return "", false
		}
		return ac.TenantSlug, true
	}
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return "", false
	}
	return "", true
}

// traceListResponse backs GET /traces/requests.json — a paginated list
// of recent traces. Total is the uncapped count (or, with ?grep=, the
// match count) so the CLI can show how much was elided.
type traceListResponse struct {
	Traces []traceListEntry `json:"traces"`
	Total  int              `json:"total"`
}

type traceListEntry struct {
	RID        string `json:"rid"`
	Src        string `json:"src,omitempty"`
	Tenant     string `json:"tenant,omitempty"`
	Stack      string `json:"stack,omitempty"`
	Route      string `json:"route,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMs *int64 `json:"duration_ms,omitempty"`
	Status     string `json:"status"`
}

const (
	traceListDefaultLimit = 50
	traceListMaxLimit     = 500
)

// validRID matches the character class hxid emits (alphanumerics) plus
// legacy `_-`. Bad rids are rejected up-front (400) before they reach
// the reader.
var validRID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type traceRequestResponse struct {
	RID              string             `json:"rid"`
	Src              string             `json:"src,omitempty"`
	Tenant           string             `json:"tenant,omitempty"`
	Stack            string             `json:"stack,omitempty"`
	Route            string             `json:"route,omitempty"`
	StartedAt        string             `json:"started_at,omitempty"`
	FinishedAt       string             `json:"finished_at,omitempty"`
	DurationMs       *int64             `json:"duration_ms,omitempty"`
	Status           string             `json:"status"`
	PayloadBytes     int64              `json:"payload_bytes,omitempty"`
	PayloadTruncated bool               `json:"payload_truncated,omitempty"`
	Fuel             int64              `json:"fuel,omitempty"`
	BytesIn          int64              `json:"bytes_in,omitempty"`
	BytesOut         int64              `json:"bytes_out,omitempty"`
	TraceMode        string             `json:"trace_mode,omitempty"`
	Steps            []traceStep        `json:"steps"`
	In               map[string]any     `json:"in,omitempty"`
	Out              any                `json:"out,omitempty"`
	Continuation     *traceContinuation `json:"continuation,omitempty"`
}

type traceContinuation struct {
	Self              string                   `json:"self"` // "origin" | "resume"
	RunContinuationID string                   `json:"run_continuation_id,omitempty"`
	OriginRID         string                   `json:"origin_rid,omitempty"`
	Resumes           []continuation.ResumeRef `json:"resumes,omitempty"`
}

type traceStep struct {
	Name            string `json:"name"`
	Operation       string `json:"operation,omitempty"`
	Transport       string `json:"transport,omitempty"`
	Stack           string `json:"stack,omitempty"`
	Scope           int    `json:"scope"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	DurationMs      int64  `json:"duration_ms"`
	Status          string `json:"status"`
	InputBytes      int64  `json:"input_bytes"`
	OutputBytes     int64  `json:"output_bytes"`
	InputTruncated  bool   `json:"input_truncated,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
	In              any    `json:"in,omitempty"`
	Out             any    `json:"out,omitempty"`
}

// traceRdr returns the trace reader, lazily building it from config if
// Start() didn't (e.g. unit tests that call handlers directly). Empty
// TraceStore defaults to "file" — the same effective default the flag
// loader applies, so a config.Config literal without TraceStore behaves
// like a real boot.
func (c *Controller) traceRdr() (trace.Reader, error) {
	if c.traceReader != nil {
		return c.traceReader, nil
	}
	store := c.pu.Conf.TraceStore
	if store == "" {
		store = "file"
	}
	tr, err := trace.OpenReader(store, trace.StoreConfig{
		Dir:  c.pu.Conf.TraceDir,
		Mode: trace.ParseMode(c.pu.Conf.TraceMode),
	})
	if err != nil {
		return nil, err
	}
	c.traceReader = tr
	return tr, nil
}

// handleTraceRequest returns one aggregated JSON document for a single
// request id. 404 when absent; 400 for malformed rids. The aggregation
// itself comes from the trace.Reader (filesystem by default; a non-fs
// backend when admin is a separate machine) — only the continuation
// cross-links are composed here from the run store.
func (c *Controller) handleTraceRequest(w http.ResponseWriter, r *http.Request) {
	tenant, ok := c.traceTenantScope(w, r)
	if !ok {
		return
	}
	rid := mux.Vars(r)["rid"]
	if !validRID.MatchString(rid) {
		http.Error(w, "invalid rid", http.StatusBadRequest)
		return
	}

	full := r.URL.Query().Get("include") == "full"

	rdr, rerr := c.traceRdr()
	if rerr != nil {
		http.Error(w, "trace read error", http.StatusInternalServerError)
		return
	}
	d, err := rdr.Get(r.Context(), rid, full)
	if err != nil {
		if errors.Is(err, trace.ErrNotFound) {
			http.Error(w, "trace not found", http.StatusNotFound)
			return
		}
		http.Error(w, "trace read error", http.StatusInternalServerError)
		return
	}
	// Tenant-scoped callers may only read their own tenant's traces; 404
	// (not 403) so a cross-tenant rid isn't even confirmed to exist.
	if tenant != "" && d.Tenant != tenant {
		http.Error(w, "trace not found", http.StatusNotFound)
		return
	}

	resp := traceRequestResponse{
		RID:              d.RID,
		Src:              d.Src,
		Tenant:           d.Tenant,
		Stack:            d.Stack,
		Route:            d.Route,
		StartedAt:        d.StartedAt,
		FinishedAt:       d.FinishedAt,
		DurationMs:       d.DurationMs,
		Status:           d.Status,
		PayloadBytes:     d.PayloadBytes,
		PayloadTruncated: d.PayloadTruncated,
		Fuel:             d.Fuel,
		BytesIn:          d.PayloadBytes,
		BytesOut:         d.BytesOut,
		TraceMode:        c.pu.Conf.TraceMode,
		Steps:            make([]traceStep, 0, len(d.Steps)),
		In:               d.In,
		Out:              d.Out,
	}
	if resp.RID == "" {
		resp.RID = rid
	}
	if resp.Status == "" {
		resp.Status = "in-flight"
	}
	for _, s := range d.Steps {
		resp.Steps = append(resp.Steps, traceStep{
			Name:            s.Name,
			Operation:       s.Operation,
			Transport:       s.Transport,
			Stack:           s.Stack,
			Scope:           s.Scope,
			StartedAt:       s.StartedAt,
			FinishedAt:      s.FinishedAt,
			DurationMs:      s.DurationMs,
			Status:          s.Status,
			InputBytes:      s.InputBytes,
			OutputBytes:     s.OutputBytes,
			InputTruncated:  s.InputTruncated,
			OutputTruncated: s.OutputTruncated,
			Error:           s.Error,
			In:              s.In,
			Out:             s.Out,
		})
	}

	// Continuation cross-links (O(1) store lookups; skipped when
	// continuations are disabled or this trace isn't part of a run).
	if runs := c.pu.Runs; runs != nil {
		var linkRunID, self string
		if rID, ok := continuation.ParseResumeTraceRID(rid); ok {
			linkRunID, self = rID, "resume"
		} else if id, e := runs.ResolveRequestContinuation(r.Context(), rid); e == nil && id != "" {
			linkRunID, self = id, "origin"
		}
		if linkRunID != "" {
			if tl, e := runs.ReadTraceLinks(r.Context(), linkRunID); e == nil {
				resp.Continuation = &traceContinuation{
					Self:              self,
					RunContinuationID: tl.RunContinuationID,
					OriginRID:         tl.OriginRID,
					Resumes:           tl.Resumes,
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// handleTraceList returns up to ?limit=N (default 50, max 500) recent
// traces, newest first. ?grep=<pattern> filters (case-insensitive
// substring across the persisted artifacts). ETag/If-None-Match drives
// the long-poll 304. All of that is the reader's job; this handler only
// parses params, echoes the (opaque) ETag, and maps to the wire shape.
func (c *Controller) handleTraceList(w http.ResponseWriter, r *http.Request) {
	tenant, ok := c.traceTenantScope(w, r)
	if !ok {
		return
	}
	limit := traceListDefaultLimit
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
			if limit > traceListMaxLimit {
				limit = traceListMaxLimit
			}
		}
	}

	rdr, rerr := c.traceRdr()
	if rerr != nil {
		http.Error(w, "list error", http.StatusInternalServerError)
		return
	}
	res, err := rdr.List(r.Context(), trace.ListQuery{
		Limit:       limit,
		Grep:        r.URL.Query().Get("grep"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
		Tenant:      tenant,
	})
	if err != nil {
		http.Error(w, "list error", http.StatusInternalServerError)
		return
	}

	if res.ETag != "" {
		w.Header().Set("ETag", res.ETag)
		w.Header().Set("Cache-Control", "no-cache") // force revalidation per RFC 7234
		if res.NotModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	resp := traceListResponse{
		Traces: make([]traceListEntry, 0, len(res.Traces)),
		Total:  res.Total,
	}
	for _, s := range res.Traces {
		resp.Traces = append(resp.Traces, traceListEntry{
			RID:        s.RID,
			Src:        s.Src,
			Tenant:     s.Tenant,
			Stack:      s.Stack,
			Route:      s.Route,
			StartedAt:  s.StartedAt,
			FinishedAt: s.FinishedAt,
			DurationMs: s.DurationMs,
			Status:     s.Status,
		})
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}
