package trace

import (
	"context"
	"errors"
	"net/http"
	"sort"
)

// ErrNotFound is returned by Reader.Get when the rid is absent.
var ErrNotFound = errors.New("trace: request not found")

// ListQuery is the read-side query for the trace list. IfNoneMatch is
// the client's cached ETag; backends that can cheaply detect "unchanged"
// set ListResult.NotModified so the handler can return 304 without a
// body. Limit/Grep mirror the ?limit=/?grep= params.
type ListQuery struct {
	Limit       int
	Grep        string
	IfNoneMatch string
}

// Summary is one row in the trace list (the wire shape the admin list
// endpoint emits; the admin handler copies these fields verbatim into
// its JSON struct).
type Summary struct {
	RID        string
	Src        string
	Tenant     string
	Stack      string
	Route      string
	StartedAt  string
	FinishedAt string
	DurationMs *int64
	Status     string
}

// Step is one op execution in the aggregated detail.
type Step struct {
	Name            string
	Operation       string
	Transport       string
	Stack           string
	Scope           int
	StartedAt       string
	FinishedAt      string
	DurationMs      int64
	Status          string
	InputBytes      int64
	OutputBytes     int64
	InputTruncated  bool
	OutputTruncated bool
	Error           string
	In              any
	Out             any
}

// RequestDetail is the aggregated per-request document (everything the
// admin detail endpoint returns except the continuation cross-links,
// which the admin layer composes from the run store — kept out of here
// so the trace package doesn't depend on chassis/continuation).
type RequestDetail struct {
	RID              string
	Src              string
	Tenant           string
	Stack            string
	Route            string
	StartedAt        string
	FinishedAt       string
	DurationMs       *int64
	Status           string
	PayloadBytes     int64
	PayloadTruncated bool
	Steps            []Step
	In               map[string]any
	Out              any
}

// ListResult is the aggregated list response. ETag is an OPAQUE cursor:
// the file backend uses an fs-stat fingerprint; a DB/index backend uses
// e.g. max ingest-id + count. The admin layer treats it as opaque and
// only echoes/compares it. NotModified ⇒ Traces is empty and the
// handler should emit 304.
type ListResult struct {
	Traces      []Summary
	Total       int
	ETag        string
	NotModified bool
}

// Reader is the read side of a trace backend. The admin endpoints go
// through this instead of the filesystem directly, so a separate-machine
// admin can read traces a chassis shipped to a central store. The
// built-in "file" reader preserves byte-for-byte the legacy fs behavior;
// "noop" returns empty/NotFound.
type Reader interface {
	// List returns newest-first summaries (≤ q.Limit), an opaque ETag,
	// and Total (full count, or match count when q.Grep != "").
	List(ctx context.Context, q ListQuery) (ListResult, error)

	// Get aggregates one request; full also embeds payloads. ErrNotFound
	// when rid is absent.
	Get(ctx context.Context, rid string, full bool) (RequestDetail, error)

	// IndexNames returns the newest ≤ max request ids plus the total
	// count, for the minimal HTML index page.
	IndexNames(ctx context.Context, max int) (names []string, total int, err error)

	// RawFS exposes the raw artifact tree for the browse-it file server,
	// when the backend is filesystem-shaped. (nil,false) ⇒ the admin
	// serves 404 for the raw path (non-fs backends).
	RawFS() (http.FileSystem, bool)
}

// ReaderConstructor builds a Reader from StoreConfig (same config the
// Sink side gets).
type ReaderConstructor func(StoreConfig) (Reader, error)

var readerRegistry = map[string]ReaderConstructor{}

// RegisterReader registers a named Reader backend (called from init();
// built-ins register from this package, so no blank import is needed).
func RegisterReader(name string, c ReaderConstructor) { readerRegistry[name] = c }

// OpenReader constructs the named Reader; unknown name is a hard error.
func OpenReader(name string, cfg StoreConfig) (Reader, error) {
	c, ok := readerRegistry[name]
	if !ok {
		avail := make([]string, 0, len(readerRegistry))
		for k := range readerRegistry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, &unknownReaderError{name: name, avail: avail}
	}
	return c(cfg)
}

type unknownReaderError struct {
	name  string
	avail []string
}

func (e *unknownReaderError) Error() string {
	return "trace: unknown reader " + e.name + " (available: " + joinStrings(e.avail) + ")"
}

func joinStrings(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += " "
		}
		out += v
	}
	return out
}

func init() {
	RegisterReader("noop", func(StoreConfig) (Reader, error) { return noopReader{}, nil })
}

// noopReader is the reader for the noop backend / trace-off: nothing was
// written, so the list is empty and every rid is NotFound.
type noopReader struct{}

func (noopReader) List(context.Context, ListQuery) (ListResult, error) {
	return ListResult{Traces: []Summary{}}, nil
}
func (noopReader) Get(context.Context, string, bool) (RequestDetail, error) {
	return RequestDetail{}, ErrNotFound
}
func (noopReader) IndexNames(context.Context, int) ([]string, int, error) { return nil, 0, nil }
func (noopReader) RawFS() (http.FileSystem, bool)                         { return nil, false }
