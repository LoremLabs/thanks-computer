package notebook

import (
	"errors"
	"fmt"
)

// CodedError carries a stable txco_notebook_* code. The op layer surfaces
// it as `<into>.error.{code,message}` with a nil Go error, so an author
// branches with `WHEN ._notebook.error.code != ""` and the run continues —
// a record that silently fails is worse than no record.
type CodedError interface {
	error
	Code() string
}

// ErrorCode returns the code carried anywhere in err's chain, or "".
func ErrorCode(err error) string {
	var ce CodedError
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return ""
}

// InvalidArgError flags a malformed request: a bad tenant or namespace, an
// empty or ill-formed type, data that is not JSON or not UTF-8, a negative
// window, since >= until, or a cursor that does not decode.
type InvalidArgError struct{ Reason string }

func (e *InvalidArgError) Error() string { return "notebook: invalid argument: " + e.Reason }
func (e *InvalidArgError) Code() string  { return "txco_notebook_invalid_arg" }

// InvalidNameError flags a notebook name outside the grammar (see
// ValidName).
type InvalidNameError struct{ Reason string }

func (e *InvalidNameError) Error() string { return "notebook: " + e.Reason }
func (e *InvalidNameError) Code() string  { return "txco_notebook_invalid_name" }

// TooLargeError flags a field over its byte cap. Its own code so an author
// can branch "trim the payload and retry" apart from "malformed".
type TooLargeError struct {
	Field string
	Max   int
	Got   int
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("notebook: %s exceeds %d bytes (%d)", e.Field, e.Max, e.Got)
}
func (e *TooLargeError) Code() string { return "txco_notebook_too_large" }

// StaleCursorError is returned when a cursor's generation is not the
// notebook's current one: the notebook was deleted and recreated since the
// cursor was issued, so continuing would silently re-read new entries as
// old. Restart from the beginning.
type StaleCursorError struct{}

func (e *StaleCursorError) Error() string {
	return "notebook: cursor belongs to an earlier generation of this notebook (it was deleted and recreated); restart from the beginning"
}
func (e *StaleCursorError) Code() string { return "txco_notebook_stale_cursor" }

// StoreError wraps a database or driver failure.
type StoreError struct {
	Op  string
	Err error
}

func (e *StoreError) Error() string { return "notebook: " + e.Op + ": " + e.Err.Error() }
func (e *StoreError) Code() string  { return "txco_notebook_store" }
func (e *StoreError) Unwrap() error { return e.Err }
