package search

import "fmt"

// CodedError carries a stable txco_search_* code surfaced on the op envelope's
// `search.error.code` so rule authors dispatch uniformly with
// `WHEN @search.error EXEC ...`.
type CodedError interface {
	error
	Code() string
}

// Codes the op layer raises itself, for conditions no backend sees.
const (
	CodeDisabled = "txco_search_disabled"
	CodeNoTenant = "txco_search_no_tenant"
	// CodeStore is the code of any backend error that carries none.
	CodeStore = "txco_search_store"
)

// CollectionNotFoundError is returned by Upsert/Query/Delete/Update for an
// unknown (tenant, collection).
type CollectionNotFoundError struct {
	Tenant     string
	Collection string
}

func (e *CollectionNotFoundError) Error() string {
	return fmt.Sprintf("search collection %q not found (ensure it with txco://search/collection first)", e.Collection)
}
func (e *CollectionNotFoundError) Code() string { return "txco_search_collection_not_found" }

// InvalidArgError is a malformed request: a missing id, an unknown filter op,
// an update with no filter.
type InvalidArgError struct{ Reason string }

func (e *InvalidArgError) Error() string { return "search: " + e.Reason }
func (e *InvalidArgError) Code() string  { return "txco_search_invalid_arg" }

// TooLargeError is a request past one of the limits in limits.go.
type TooLargeError struct {
	What  string
	Got   int
	Limit int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("search: %s is %d (limit %d)", e.What, e.Got, e.Limit)
}
func (e *TooLargeError) Code() string { return "txco_search_too_large" }

// AnalyzerMismatchError is returned when a collection is pinned to an analyzer
// or scoring model other than the one asked for, or than this build provides.
// The fix is a re-index of the collection, never an edit of the pin.
type AnalyzerMismatchError struct {
	Collection string
	Field      string // "analyzer_version" | "scoring_model"
	Existing   string
	Requested  string
}

func (e *AnalyzerMismatchError) Error() string {
	return fmt.Sprintf("search collection %q is pinned to %s=%q, not %q (re-index the collection to change it)",
		e.Collection, e.Field, e.Existing, e.Requested)
}
func (e *AnalyzerMismatchError) Code() string { return "txco_search_analyzer_mismatch" }

// UnavailableError is returned by a backend that cannot reach its engine right
// now. A stack should treat it as "no lexical results", not as a failure of
// the request.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string { return "search unavailable: " + e.Reason }
func (e *UnavailableError) Code() string  { return "txco_search_unavailable" }

// RecoveringError is returned by a backend whose engine is up but has not yet
// caught up, so an answer now would be silently incomplete.
type RecoveringError struct{ Reason string }

func (e *RecoveringError) Error() string { return "search recovering: " + e.Reason }
func (e *RecoveringError) Code() string  { return "txco_search_recovering" }
