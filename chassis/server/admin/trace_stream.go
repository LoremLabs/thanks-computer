package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// traceStreamEvent is the per-trace wire shape on /traces/stream. It
// mirrors traceRequestResponse minus the continuation cross-links
// (those are an O(1)-per-rid store lookup; out of the hot streaming
// path — clients can resolve them by opening /traces/requests/{rid}.
// json on click) and plus an opaque per-subscription cursor.
type traceStreamEvent struct {
	RID              string `json:"rid"`
	Src              string `json:"src,omitempty"`
	Tenant           string `json:"tenant,omitempty"`
	Stack            string `json:"stack,omitempty"`
	Route            string `json:"route,omitempty"`
	StartedAt        string `json:"started_at,omitempty"`
	FinishedAt       string `json:"finished_at,omitempty"`
	DurationMs       *int64 `json:"duration_ms,omitempty"`
	Status           string `json:"status"`
	PayloadBytes     int64  `json:"payload_bytes,omitempty"`
	PayloadTruncated bool   `json:"payload_truncated,omitempty"`
	// Fuel/BytesIn/BytesOut mirror the archive detail response
	// (traceRequestResponse): per-request fuel + request/response sizes,
	// lifted from the request.usage timeline event. Without these the
	// admin's "fuel" row stays blank on the live path — which, on the
	// NATS/R2 backend, is the ONLY path that serves a successful trace
	// (the archive is on-error only). BytesIn is the payload size.
	Fuel     int64          `json:"fuel,omitempty"`
	BytesIn  int64          `json:"bytes_in,omitempty"`
	BytesOut int64          `json:"bytes_out,omitempty"`
	Steps    []traceStep    `json:"steps"`
	In       map[string]any `json:"in,omitempty"`
	Out      any            `json:"out,omitempty"`
	Cursor   string         `json:"cursor"`
}

// traceStreamResponse is the body of a 200 response: a batch of new
// events. NextCursor is the cursor of the last event (clients echo
// this back on the next request); the last event's per-event Cursor
// is the authoritative value, NextCursor is just convenience.
type traceStreamResponse struct {
	Events     []traceStreamEvent `json:"events"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// handleTraceStream is the live-trace long-poll endpoint. Holds the
// request open up to `wait` ms (clamped to TraceStreamLongPollMS and
// the request's own deadline minus 1.5s), subscribes to the
// configured Armable, and returns either:
//   - 200 with a batch of new events when any arrive; or
//   - 202 (with Retry-After) when the deadline fires with no events
//     so the client re-polls with the same cursor.
//
// Cursor handling is opaque end-to-end: the client echoes whatever
// cursor it last received; the Armable's Subscribe receives that
// cursor and is responsible for delivering only events strictly
// newer. Best-effort live tail; events older than the subscriber's
// join moment are NOT retroactively delivered (durable replay is a
// JetStream upgrade, deliberately out of v1 scope).
func (c *Controller) handleTraceStream(w http.ResponseWriter, r *http.Request) {
	if c.traceArmable == nil {
		http.Error(w, "trace stream not available on this backend", http.StatusNotFound)
		return
	}
	// Authorize + scope: tenant-scoped callers stream only their own tenant's
	// traces; flat callers must be super-admin (chassis-wide, no filter).
	tenant, ok := c.traceTenantScope(w, r)
	if !ok {
		return
	}

	cursor := r.URL.Query().Get("cursor")

	maxWait := c.pu.Conf.TraceStreamLongPollMS
	if maxWait <= 0 {
		maxWait = 30000
	}
	wait := maxWait
	if q := r.URL.Query().Get("wait"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 {
			wait = n
			if wait > maxWait {
				wait = maxWait
			}
		}
	}

	// Budget = min(wait, ctx.Deadline − 1.5s) so we always return in
	// time for the caller to emit a clean 202 before the web write
	// timeout would otherwise kill the request. Mirrors
	// chassis/server/server.go's awaitTerminalState pattern.
	budget := time.Duration(wait) * time.Millisecond
	if dl, ok := r.Context().Deadline(); ok {
		if d := time.Until(dl) - 1500*time.Millisecond; d < budget {
			budget = d
		}
	}
	if budget <= 0 {
		writeTraceStreamTimeout(w)
		return
	}

	bufHint := c.pu.Conf.TraceStreamRingSize
	if bufHint <= 0 {
		bufHint = 1024
	}
	sub, err := c.traceArmable.Subscribe(r.Context(), cursor, bufHint)
	if err != nil {
		http.Error(w, "subscribe error", http.StatusInternalServerError)
		return
	}
	defer sub.Close()

	// Batch sizing: collect up to maxBatch events or until a brief
	// quiet window (flushIdle) elapses with no further events —
	// whichever is first. Keeps responses snappy when traffic is
	// light without churning on each event.
	const maxBatch = 50
	const flushIdle = 50 * time.Millisecond

	deadline := time.After(budget)
	kept := make([]traceStreamEvent, 0, 8)
	var lastCursor string // advances over ALL seen events, incl. filtered ones
	seen := false

	// Wait up to the full budget for the first event of ANY tenant; once one
	// arrives, switch to a short flushIdle window to greedily drain. Tenant-
	// scoped callers keep only their own events but still advance lastCursor
	// past foreign ones, so a quiet tenant on a busy chassis doesn't re-poll
	// the same foreign events every long-poll cycle.
	for {
		var idle <-chan time.Time
		if seen {
			idle = time.After(flushIdle)
		}
		select {
		case <-r.Context().Done():
			flushTraceStream(w, kept, lastCursor, seen)
			return
		case <-deadline:
			flushTraceStream(w, kept, lastCursor, seen)
			return
		case <-idle:
			flushTraceStream(w, kept, lastCursor, seen)
			return
		case ev, more := <-sub.Events():
			if !more {
				flushTraceStream(w, kept, lastCursor, seen)
				return
			}
			seen = true
			lastCursor = ev.Cursor
			if tenant != "" && ev.Tenant != tenant {
				continue
			}
			kept = append(kept, closedTraceToWire(ev))
			if len(kept) >= maxBatch {
				flushTraceStream(w, kept, lastCursor, seen)
				return
			}
		}
	}
}

// flushTraceStream writes the long-poll response: 202 when no event was seen
// at all (client re-polls with the same cursor); otherwise a 200 batch with
// the cursor advanced past every seen event — even when `events` is empty
// because a tenant-scoped caller filtered them all out.
func flushTraceStream(w http.ResponseWriter, events []traceStreamEvent, nextCursor string, seen bool) {
	if !seen {
		writeTraceStreamTimeout(w)
		return
	}
	writeTraceStreamBatch(w, events, nextCursor)
}

func writeTraceStreamTimeout(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusAccepted)
}

func writeTraceStreamBatch(w http.ResponseWriter, events []traceStreamEvent, nextCursor string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	resp := traceStreamResponse{Events: events, NextCursor: nextCursor}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// closedTraceToWire maps a trace.ClosedTrace (embedding
// trace.RequestDetail, plus an opaque Cursor) into the admin's wire
// shape. Mirrors the same field copy handleTraceRequest does for the
// archive endpoint — kept inline for v1 simplicity; refactor into a
// shared helper if a third caller ever appears.
func closedTraceToWire(t trace.ClosedTrace) traceStreamEvent {
	ev := traceStreamEvent{
		RID:              t.RID,
		Src:              t.Src,
		Tenant:           t.Tenant,
		Stack:            t.Stack,
		Route:            t.Route,
		StartedAt:        t.StartedAt,
		FinishedAt:       t.FinishedAt,
		DurationMs:       t.DurationMs,
		Status:           t.Status,
		PayloadBytes:     t.PayloadBytes,
		PayloadTruncated: t.PayloadTruncated,
		Fuel:             t.Fuel,
		BytesIn:          t.PayloadBytes,
		BytesOut:         t.BytesOut,
		Steps:            make([]traceStep, 0, len(t.Steps)),
		In:               t.In,
		Out:              t.Out,
		Cursor:           t.Cursor,
	}
	if ev.Status == "" {
		ev.Status = "in-flight"
	}
	for _, s := range t.Steps {
		ev.Steps = append(ev.Steps, traceStep{
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
	return ev
}
