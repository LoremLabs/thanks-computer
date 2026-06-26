package vector

import "fmt"

// CodedError carries a stable txco_vector_* code surfaced on the op envelope's
// `vector.error.code` so rule authors dispatch uniformly with
// `WHEN @vector.error EXEC ...`.
type CodedError interface {
	error
	Code() string
}

// CollectionNotFoundError is returned by Upsert/Search/Delete for an unknown
// (tenant, collection).
type CollectionNotFoundError struct {
	Tenant     string
	Collection string
}

func (e *CollectionNotFoundError) Error() string {
	return fmt.Sprintf("vector: collection %q not found for tenant %q (create it first)", e.Collection, e.Tenant)
}
func (e *CollectionNotFoundError) Code() string { return "txco_vector_collection_not_found" }

// CollectionConflictError is returned by EnsureCollection when the requested
// pin differs from the existing one (a model/dimension/metric change is a new
// collection + re-embed, never an in-place mutation).
type CollectionConflictError struct {
	Collection string
	Field      string // "dimensions" | "embedding_model" | "metric"
	Existing   string
	Requested  string
}

func (e *CollectionConflictError) Error() string {
	return fmt.Sprintf("vector: collection %q already pinned %s=%s, cannot change to %s (version the collection instead)",
		e.Collection, e.Field, e.Existing, e.Requested)
}
func (e *CollectionConflictError) Code() string { return "txco_vector_collection_conflict" }

// DimensionMismatchError is returned when a vector's length differs from the
// collection's pinned dimension.
type DimensionMismatchError struct {
	Collection string
	Want       int
	Got        int
}

func (e *DimensionMismatchError) Error() string {
	return fmt.Sprintf("vector: collection %q expects %d-dim vectors, got %d", e.Collection, e.Want, e.Got)
}
func (e *DimensionMismatchError) Code() string { return "txco_vector_dimension_mismatch" }

// InvalidArgError flags a malformed request (empty collection name,
// non-positive dimension, unsupported metric, empty id).
type InvalidArgError struct {
	Reason string
}

func (e *InvalidArgError) Error() string { return "vector: invalid argument: " + e.Reason }
func (e *InvalidArgError) Code() string  { return "txco_vector_invalid_arg" }
