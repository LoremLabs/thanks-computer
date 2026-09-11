// Package notebook is the append-only record a stack writes and reads
// through txco://notebook/*: per (tenant, namespace, name) a head row that
// allocates sequence numbers, and entries {seq, at, type, data, object_key}
// in that order. Task is state; a notebook is history — "how did we get
// here?" for a task, a conversation, a workspace session, an import.
//
// Storage is dialect-aware (registry.Dialect, the same seam imap, auth and
// the scheduled store use); the bundled backend is a SQLite file of its
// own. It is deliberately NOT a set of runtime tables: the dbcache watcher
// reloads the whole runtime mirror on any runtime-DB write, and a notebook
// is a high-write table by definition.
//
// Invariants: seq is allocated from the head row's next_seq — stored, never
// MAX(seq)+1 — so a pruned tail never recycles a sequence number; `at` is
// assigned here at commit and never by the caller; every read returns
// entries ascending by seq; a duplicate object_key returns the original row
// unchanged; and a public cursor is opaque — it carries the head's
// generation, so a cursor from a deleted-and-recreated notebook fails
// loudly (StaleCursorError) instead of silently re-reading new entries as
// old ones.
//
// A notebook write is not part of the transaction of the thing it
// describes: it is an application record, not an event-sourcing substrate.
package notebook

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// AtLayout is the canonical, FIXED-WIDTH UTC timestamp stored in `at`,
// `expires_at`, `created_at` and `updated_at`: 30 bytes always (the
// fraction keeps its zeros, the zone is a literal Z), so a TEXT comparison
// on either engine is a chronological comparison. time.RFC3339Nano trims
// trailing zeros and would sort "…00Z" after "…00.5Z".
const AtLayout = "2006-01-02T15:04:05.000000000Z"

// FormatAt renders t in AtLayout, in UTC.
func FormatAt(t time.Time) string { return t.UTC().Format(AtLayout) }

// ParseAt reads an AtLayout timestamp, accepting any RFC 3339 precision as
// a fallback so caller-supplied `since`/`until` normalise through it.
func ParseAt(s string) (time.Time, error) {
	if t, err := time.Parse(AtLayout, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// Ref identifies a notebook: unique within (tenant, namespace), never
// addressable across tenants. Tenant and namespace are single segments
// (the KV grammar: no '/', no control characters); the name follows the
// blob grammar so hierarchical names like task/42 work and prefixes are
// enumerable.
type Ref struct {
	Tenant    string
	Namespace string
	Name      string
}

// ID is the composed row key, stored whole (not hashed) so the table stays
// legible to an operator and List stays a bounded scan.
func (r Ref) ID() string { return r.Tenant + "/" + r.Namespace + "/" + r.Name }

// Validate reports why r is not a well-formed reference (nil = valid).
func (r Ref) Validate() error {
	if !segOK(r.Tenant) {
		return &InvalidArgError{Reason: "invalid tenant scope"}
	}
	if !segOK(r.Namespace) {
		return &InvalidArgError{Reason: "invalid namespace " + strconv.Quote(r.Namespace)}
	}
	return ValidName(r.Name)
}

// Entry is one notebook row. Data is always a valid JSON document ("{}"
// when the append omitted it). ExpiresAt is zero for an unbounded entry.
type Entry struct {
	Seq       int64
	At        time.Time
	Type      string
	Data      json.RawMessage
	ObjectKey string
	ExpiresAt time.Time
}

// Limits are the node-configured caps. Zero fields take DefaultLimits.
type Limits struct {
	// MaxDataBytes caps one entry's `data` (the KV --kv-max-value-bytes
	// posture); 0 = unlimited.
	MaxDataBytes int
	// DefaultReadLimit is the page size when a read names none.
	DefaultReadLimit int
	// MaxReadLimit is the page ceiling; larger requests are clamped.
	MaxReadLimit int
	// MaxTTL clamps a requested TTL; 0 = unlimited.
	MaxTTL time.Duration
}

// DefaultLimits are the bundled defaults.
func DefaultLimits() Limits {
	return Limits{MaxDataBytes: 65536, DefaultReadLimit: 100, MaxReadLimit: 1000}
}

// AppendReq is one append. TTL > 0 overrides the head's ttl_secs for this
// entry only; 0 inherits it.
type AppendReq struct {
	Ref       Ref
	Type      string
	Data      json.RawMessage
	ObjectKey string
	TTL       time.Duration
}

// AppendResult reports the entry's position. Existed means the object_key
// was already recorded: Seq and At are the ORIGINAL row's and nothing was
// written, so a retry is observationally identical to the first success.
type AppendResult struct {
	Seq     int64
	At      time.Time
	Existed bool
}

// Cursor is a position in a notebook. On the wire it is an opaque string
// (Encode); callers never build one from a seq. It carries the head's
// generation so a cursor outlives neither a delete nor a recreate.
type Cursor struct {
	Generation int64
	Seq        int64
}

// IsZero reports the start-of-notebook cursor (the empty string).
func (c Cursor) IsZero() bool { return c.Generation == 0 && c.Seq == 0 }

const cursorVersion = "1"

// Encode renders the cursor as an opaque, URL- and header-safe string:
// a version character followed by base64url (no padding) of
// "<generation>:<seq>". The zero cursor encodes as "".
func (c Cursor) Encode() string {
	if c.IsZero() {
		return ""
	}
	raw := strconv.FormatInt(c.Generation, 10) + ":" + strconv.FormatInt(c.Seq, 10)
	return cursorVersion + base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor reads an Encode'd cursor; "" is the zero cursor. Anything
// else that does not decode — including a bare sequence number — is an
// InvalidArgError, so the format can evolve behind the version character.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	if !strings.HasPrefix(s, cursorVersion) {
		return Cursor{}, &InvalidArgError{Reason: "malformed cursor"}
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[len(cursorVersion):])
	if err != nil {
		return Cursor{}, &InvalidArgError{Reason: "malformed cursor"}
	}
	g, q, ok := strings.Cut(string(raw), ":")
	if !ok {
		return Cursor{}, &InvalidArgError{Reason: "malformed cursor"}
	}
	gen, err1 := strconv.ParseInt(g, 10, 64)
	seq, err2 := strconv.ParseInt(q, 10, 64)
	if err1 != nil || err2 != nil || gen <= 0 || seq <= 0 {
		return Cursor{}, &InvalidArgError{Reason: "malformed cursor"}
	}
	return Cursor{Generation: gen, Seq: seq}, nil
}

// newGeneration mints a head generation: 62 random bits, never zero.
// Random rather than clock-derived so a recreate under a pinned or coarse
// clock still gets a fresh value.
func newGeneration() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	g := int64(binary.BigEndian.Uint64(b[:]) >> 2)
	if g == 0 {
		g = 1
	}
	return g, nil
}

// ReadReq selects entries. Selection varies; the order never does — every
// result is ascending by seq.
//
//   - After: exclusive cursor (the head walk); zero = from the start.
//   - Since/Until: `at` in [Since, Until); zero = unbounded on that side.
//   - Tail: the newest N of the selection, still returned ascending.
//   - Type: only entries of that type.
//   - Limit: page cap; 0 = DefaultReadLimit, clamped to MaxReadLimit.
//   - UpTo: an inclusive seq ceiling ForEach uses to snapshot a walk. Not
//     on the op surface.
type ReadReq struct {
	Ref   Ref
	After Cursor
	UpTo  int64
	Since time.Time
	Until time.Time
	Tail  int
	Type  string
	Limit int
}

// ReadResult is one page. Next is the cursor for the following page and is
// non-zero only when the page was full (the LOOP … UNTIL next == "" drain
// idiom); in tail mode it is always zero, because rows clipped there are
// older than the window and not reachable by a forward cursor. Cursor is
// the position after the last returned entry whenever Count > 0 — what a
// poller feeds back as After — and equals Next when Next is non-zero.
type ReadResult struct {
	Entries   []Entry
	Next      Cursor
	Cursor    Cursor
	Count     int
	Truncated bool
}

// Head is a notebook's head row. HighSeq is the last allocated sequence
// number (0 = never appended); TTL is the default retention for new
// entries (0 = unbounded).
type Head struct {
	Name       string
	Generation int64
	HighSeq    int64
	TTL        time.Duration
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ListResult is one page of heads, ascending by name. Next is the last
// name when more remain, "" otherwise.
type ListResult struct {
	Notebooks []Head
	Next      string
	Count     int
}
