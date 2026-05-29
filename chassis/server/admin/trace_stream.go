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
	RID              string         `json:"rid"`
	Src              string         `json:"src,omitempty"`
	Tenant           string         `json:"tenant,omitempty"`
	Stack            string         `json:"stack,omitempty"`
	Route            string         `json:"route,omitempty"`
	StartedAt        string         `json:"started_at,omitempty"`
	FinishedAt       string         `json:"finished_at,omitempty"`
	DurationMs       *int64         `json:"duration_ms,omitempty"`
	Status           string         `json:"status"`
	PayloadBytes     int64          `json:"payload_bytes,omitempty"`
	PayloadTruncated bool           `json:"payload_truncated,omitempty"`
	Steps            []traceStep    `json:"steps"`
	In               map[string]any `json:"in,omitempty"`
	Out              any            `json:"out,omitempty"`
	Cursor           string         `json:"cursor"`
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
	events := make([]traceStreamEvent, 0, 8)

	// First event: wait up to the full budget. After we have at least
	// one event, switch to a short flushIdle window to greedily drain
	// the channel.
	for {
		if len(events) == 0 {
			select {
			case <-r.Context().Done():
				writeTraceStreamTimeout(w)
				return
			case <-deadline:
				writeTraceStreamTimeout(w)
				return
			case ev, ok := <-sub.Events():
				if !ok {
					writeTraceStreamTimeout(w)
					return
				}
				events = append(events, closedTraceToWire(ev))
				if len(events) >= maxBatch {
					writeTraceStreamBatch(w, events)
					return
				}
			}
		} else {
			select {
			case <-r.Context().Done():
				writeTraceStreamBatch(w, events)
				return
			case <-deadline:
				writeTraceStreamBatch(w, events)
				return
			case <-time.After(flushIdle):
				writeTraceStreamBatch(w, events)
				return
			case ev, ok := <-sub.Events():
				if !ok {
					writeTraceStreamBatch(w, events)
					return
				}
				events = append(events, closedTraceToWire(ev))
				if len(events) >= maxBatch {
					writeTraceStreamBatch(w, events)
					return
				}
			}
		}
	}
}

func writeTraceStreamTimeout(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Retry-After", "0")
	w.WriteHeader(http.StatusAccepted)
}

func writeTraceStreamBatch(w http.ResponseWriter, events []traceStreamEvent) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	resp := traceStreamResponse{Events: events}
	if n := len(events); n > 0 {
		resp.NextCursor = events[n-1].Cursor
	}
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
