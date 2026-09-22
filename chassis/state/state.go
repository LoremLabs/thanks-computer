// Package state is the chassis's durable state-record primitive: a named
// record (tenant, machine, id) with a state, a version and opaque JSON
// data; an atomic compare-and-swap transition that checks BOTH the
// current state and the expected version; and a durable event for every
// committed transition, written in the same transaction (a transactional
// outbox). The `state` personality presents each event at least once
// into the tenant's `_state/0` stack.
//
// It is not a workflow engine: no machine definitions, no timers, no
// retries of the caller's work, no watchers, no deletion. See
// docs/advanced/protocols/state.md.
package state

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Grammar. `machine` plays the role a KV namespace does: a lowercase
// dotted family name the consumer owns (`onepony.task`). `id` is free
// text inside the tenant's machine (`acme:t_123`), shaped like a notebook
// name segment. States are short tokens.
const (
	MaxMachineBytes = 64
	MaxIDBytes      = 256
	MaxStateBytes   = 64
	MaxTenantBytes  = 128

	// DefaultMaxDataBytes caps `data` unless SetMaxDataBytes changes it.
	DefaultMaxDataBytes = 64 * 1024
)

var (
	machineRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)
	stateRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// AtLayout is the fixed-width UTC timestamp stored in every *_at column
// (the notebook store's), so text comparison is chronological on both
// engines.
const AtLayout = "2006-01-02T15:04:05.000000000Z"

// Event statuses. pending → claimed → done | skipped | dead; a claim that
// did not reach acceptance goes back to pending (with a next_attempt_at)
// or, past the attempt cap, to dead.
const (
	StatusPending = "pending"
	StatusClaimed = "claimed"
	StatusDone    = "done"
	StatusSkipped = "skipped"
	StatusDead    = "dead"
)

// Record is one durable state record.
type Record struct {
	Tenant    string          `json:"-"`
	Machine   string          `json:"machine"`
	ID        string          `json:"id"`
	State     string          `json:"state"`
	Version   int64           `json:"version"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Cause is the provenance of a transition: which run committed it. It is
// filled by the op layer from trusted context, never from arguments, and
// rides on the event into `_state/0` as `@state.cause.*`.
type Cause struct {
	Source string `json:"source"` // the committing run's @src (web, lmtp, scheduled, state, …)
	Stack  string `json:"stack"`  // the committing run's stack
	Trace  string `json:"trace"`  // the committing run's rid
	Run    string `json:"run"`    // its continuation run id, when it had one
}

// Event is one committed transition and its delivery bookkeeping.
type Event struct {
	ID        string    `json:"event_id"` // stev_<hxid>
	Tenant    string    `json:"-"`
	Machine   string    `json:"machine"`
	RecordID  string    `json:"id"`
	Version   int64     `json:"version"` // the version the transition produced
	From      string    `json:"from"`
	To        string    `json:"to"`
	Cause     Cause     `json:"cause"`
	CreatedAt time.Time `json:"created_at"`

	Status string `json:"status"`
	// Attempts counts presentations that consumed an attempt: an accept
	// timeout or a claim reclaimed from a dead node. A denial or a
	// refused handoff does not count. The stack sees Attempts+1 as
	// `@state.attempt`.
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"-"`
	ClaimedBy     string    `json:"-"`
	ClaimedAt     time.Time `json:"-"`
	DeliveredAt   time.Time `json:"-"`
	DeliveredRid  string    `json:"-"`
	LastError     string    `json:"-"`
}

// CreateReq creates a record at version 1. Nil Data stores `{}`.
type CreateReq struct {
	Tenant  string
	Machine string
	ID      string
	State   string
	Data    json.RawMessage
}

// TransitionReq moves a record From → To when its current state is From
// AND its version is ExpectedVersion. Nil Data keeps the stored data; any
// other value (including `{}`) replaces it. From may equal To: a
// data-bearing self-transition still bumps the version and emits an event.
type TransitionReq struct {
	Tenant          string
	Machine         string
	ID              string
	From            string
	To              string
	ExpectedVersion int64
	Data            json.RawMessage
	Cause           Cause
}

// ValidMachine reports whether m is a machine name.
func ValidMachine(m string) error {
	if m == "" {
		return &InvalidArgError{Reason: "machine is empty"}
	}
	if !machineRe.MatchString(m) {
		return &InvalidArgError{Reason: fmt.Sprintf("machine %q: want [a-z][a-z0-9._-]{0,%d}", m, MaxMachineBytes-1)}
	}
	return nil
}

// ValidID reports whether id is a record id: 1–MaxIDBytes bytes of UTF-8
// with no '/' and no control characters.
func ValidID(id string) error {
	if id == "" {
		return &InvalidArgError{Reason: "id is empty"}
	}
	if len(id) > MaxIDBytes {
		return &InvalidArgError{Reason: fmt.Sprintf("id exceeds %d bytes (%d)", MaxIDBytes, len(id))}
	}
	if !utf8.ValidString(id) {
		return &InvalidArgError{Reason: "id is not valid UTF-8"}
	}
	for _, r := range id {
		if r == '/' || r < 0x20 || r == 0x7f {
			return &InvalidArgError{Reason: "id contains '/' or a control character"}
		}
	}
	return nil
}

// ValidState reports whether st is a state token.
func ValidState(field, st string) error {
	if st == "" {
		return &InvalidArgError{Reason: field + " is empty"}
	}
	if !stateRe.MatchString(st) {
		return &InvalidArgError{Reason: fmt.Sprintf("%s %q: want [A-Za-z0-9][A-Za-z0-9._-]{0,%d}", field, st, MaxStateBytes-1)}
	}
	return nil
}

func validTenant(t string) error {
	if t == "" {
		return &InvalidArgError{Reason: "tenant is empty"}
	}
	if len(t) > MaxTenantBytes || !utf8.ValidString(t) {
		return &InvalidArgError{Reason: "tenant is malformed"}
	}
	for _, r := range t {
		if r < 0x20 || r == 0x7f {
			return &InvalidArgError{Reason: "tenant is malformed"}
		}
	}
	return nil
}

// validData checks a present data value: UTF-8 (Postgres TEXT rejects
// what json.Valid lets through) and JSON, under the cap (0 = uncapped).
func validData(data []byte, max int) error {
	if !utf8.Valid(data) {
		return &InvalidArgError{Reason: "data is not valid UTF-8"}
	}
	if !json.Valid(data) {
		return &InvalidArgError{Reason: "data is not valid JSON"}
	}
	if max > 0 && len(data) > max {
		return &InvalidArgError{Reason: fmt.Sprintf("data exceeds %d bytes (%d)", max, len(data))}
	}
	return nil
}

// formatAt renders t in AtLayout, in UTC.
func formatAt(t time.Time) string { return t.UTC().Format(AtLayout) }

// parseAt reads an AtLayout timestamp; a blank or unparseable value is the
// zero time (a column that is NULL for this row).
func parseAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(AtLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
