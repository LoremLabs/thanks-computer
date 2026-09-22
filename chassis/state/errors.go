package state

import (
	"errors"
	"fmt"
)

// CodedError carries a stable txco_state_* code. The op layer surfaces it
// as `<into>.error.{code,message}` with a nil Go error, so a stack
// branches on the code and the run continues: a conflict is ordinary
// control flow, not a failure.
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

// InvalidArgError flags a malformed request: a name outside its grammar,
// data that is not JSON or not UTF-8 or over the cap, a version below 1.
type InvalidArgError struct{ Reason string }

func (e *InvalidArgError) Error() string { return "state: invalid argument: " + e.Reason }
func (e *InvalidArgError) Code() string  { return "txco_state_invalid_arg" }

// NotFoundError: no record at (tenant, machine, id).
type NotFoundError struct{ Machine, ID string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("state: %s/%s: not found", e.Machine, e.ID)
}
func (e *NotFoundError) Code() string { return "txco_state_not_found" }

// ExistsError: Create found a record already there. Current is what is
// stored, so the caller can decide whether that is the record it wanted.
type ExistsError struct{ Current Record }

func (e *ExistsError) Error() string {
	return fmt.Sprintf("state: %s/%s: already exists at %s@%d", e.Current.Machine, e.Current.ID, e.Current.State, e.Current.Version)
}
func (e *ExistsError) Code() string { return "txco_state_exists" }

// ConflictError: the compare-and-swap did not match. Reason is "version"
// when ExpectedVersion is stale (checked first — a stale actor, doc §10)
// and "state" when the version matched but the record is not in From.
// Current is the record as it is now.
type ConflictError struct {
	Reason  string
	Current Record
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("state: %s/%s: %s conflict (now %s@%d)", e.Current.Machine, e.Current.ID, e.Reason, e.Current.State, e.Current.Version)
}
func (e *ConflictError) Code() string { return "txco_state_conflict" }

// StoreError wraps a database or driver failure.
type StoreError struct {
	Op  string
	Err error
}

func (e *StoreError) Error() string { return "state: " + e.Op + ": " + e.Err.Error() }
func (e *StoreError) Code() string  { return "txco_state_store" }
func (e *StoreError) Unwrap() error { return e.Err }
