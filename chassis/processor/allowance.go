package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tidwall/gjson"
)

// Allowances: a tenant's own fuel budgets inside its tenant budget (see
// chassis/allowance for the definitions and counters). This file is the
// request side: which allowance a request entered, the fuel ceiling that
// entry set, and the refusal when the allowance is spent. The pin lives on
// the request's budgetState, so it holds across every Run of the request,
// and rides the envelope as `_txc.allowance` (reserved: no rule can write
// it) so continuations and detached work keep it.

// AllowanceDenyReason is the admission reason a spent allowance refuses
// with (`x-txc-deny-reason` on the web head).
const AllowanceDenyReason = "allowance_exhausted"

// ErrAllowanceEntered refuses a second allowance on a request: once a
// request is in one, nothing downstream can move it to another.
var ErrAllowanceEntered = errors.New("request already entered another allowance")

// allowancePin is the allowance a request entered. limit/resetsAt are zero
// for a name with no definition (metered, never refused).
type allowancePin struct {
	name     string
	limit    int64     // the window's fuel
	resetsAt time.Time // the window's end
	cap      int64     // the request fuel ceiling this entry set; 0 = none
	denied   bool      // the window was already spent at entry
}

// envelopeAllowance is `_txc.allowance`, readable by rules as @allowance.
type envelopeAllowance struct {
	Name     string `json:"name"`
	Fuel     int64  `json:"fuel,omitempty"`
	ResetsAt string `json:"resets_at,omitempty"`
	Cap      int64  `json:"cap,omitempty"`
}

func (p *allowancePin) envelope() envelopeAllowance {
	e := envelopeAllowance{Name: p.name, Fuel: p.limit, Cap: p.cap}
	if !p.resetsAt.IsZero() {
		e.ResetsAt = p.resetsAt.UTC().Format(time.RFC3339)
	}
	return e
}

func (p *allowancePin) exhausted(used int64, stage string) *AllowanceExhaustedError {
	return &AllowanceExhaustedError{
		Allowance: p.name, Fuel: p.limit, ResetsAt: p.resetsAt,
		FuelUsed: used, LastStage: stage,
	}
}

// AllowanceExhaustedError ends a request whose allowance is spent: either
// the window was spent when it entered, or the request reached the ceiling
// the allowance set for it.
type AllowanceExhaustedError struct {
	Allowance string
	Fuel      int64
	ResetsAt  time.Time
	FuelUsed  int64
	LastStage string
}

func (e *AllowanceExhaustedError) Error() string {
	return fmt.Sprintf("txco_allowance_exhausted: allowance %q spent until %s (stage %s)",
		e.Allowance, e.ResetsAt.UTC().Format(time.RFC3339), e.LastStage)
}

// AsJSON is the response body: the error code, the allowance and when its
// window resets, and what this request burned.
func (e *AllowanceExhaustedError) AsJSON() string {
	b, _ := json.Marshal(struct {
		Code      string `json:"code"`
		Allowance string `json:"allowance"`
		Fuel      int64  `json:"fuel"`
		ResetsAt  string `json:"resets_at"`
		FuelUsed  int64  `json:"fuel_used"`
		LastStage string `json:"last_stage"`
	}{"txco_allowance_exhausted", e.Allowance, e.Fuel, e.ResetsAt.UTC().Format(time.RFC3339), e.FuelUsed, e.LastStage})
	return string(b)
}

// RetryAfter is the wait until the window resets, at least a second.
func (e *AllowanceExhaustedError) RetryAfter(now time.Time) time.Duration {
	if d := e.ResetsAt.Sub(now); d > time.Second {
		return d
	}
	return time.Second
}

// AllowanceCheck is the state of the allowance being entered, as the caller
// read it from the allowance store.
type AllowanceCheck struct {
	Name     string
	Defined  bool // false: metered, never refused
	Fuel     int64
	Used     int64
	ResetsAt time.Time
}

// EnterAllowance pins the request to an allowance. Entering is write-once:
// a request already in another allowance gets ErrAllowanceEntered, while
// entering the same one again refreshes the check. For a defined allowance
// the request's fuel ceiling is lowered to what the window has left, so one
// request cannot run far past it; a window already spent marks the request
// denied, and its next budget check (the next scope entry, or the next LOOP
// pass) ends it with AllowanceExhaustedError. denied reports that outcome.
func EnterAllowance(ctx context.Context, c AllowanceCheck) (denied bool, err error) {
	s := budgetFromCtx(ctx)
	if s == nil {
		return false, errors.New("allowance: no request budget in scope")
	}
	for {
		cur := s.allowance.Load()
		if cur != nil && cur.name != c.Name {
			return false, fmt.Errorf("%w: %q", ErrAllowanceEntered, cur.name)
		}
		p := &allowancePin{name: c.Name}
		if c.Defined {
			p.limit, p.resetsAt = c.Fuel, c.ResetsAt
			if left := c.Fuel - c.Used; left <= 0 {
				p.denied = true
			} else {
				ceiling := s.fuel.Load() + left
				if m := s.maxFuel.Load(); m <= 0 || ceiling < m {
					p.cap = ceiling
				} else if cur != nil && cur.cap == m {
					p.cap = m // an earlier entry's ceiling is still the one in force
				}
			}
		}
		if !s.allowance.CompareAndSwap(cur, p) {
			continue
		}
		if p.cap > 0 {
			s.lowerMaxFuel(p.cap)
		}
		return p.denied, nil
	}
}

// lowerMaxFuel lowers the ceiling to ceiling (0 = unlimited counts as the
// highest); it never raises it.
func (s *budgetState) lowerMaxFuel(ceiling int64) {
	for {
		m := s.maxFuel.Load()
		if m > 0 && m <= ceiling {
			return
		}
		if s.maxFuel.CompareAndSwap(m, ceiling) {
			return
		}
	}
}

// restoreAllowance re-pins the allowance a request entered before a
// continuation suspend or a detached op, from the chassis-stamped
// `_txc.allowance`, and re-applies the ceiling it set (lower only).
func restoreAllowance(s *budgetState, raw string) {
	a := gjson.Get(raw, "_txc.allowance")
	name := a.Get("name").String()
	if !a.IsObject() || name == "" {
		return
	}
	p := &allowancePin{name: name, limit: a.Get("fuel").Int()}
	if t, err := time.Parse(time.RFC3339, a.Get("resets_at").String()); err == nil {
		p.resetsAt = t
	}
	if c := a.Get("cap").Int(); c > 0 {
		if m := s.maxFuel.Load(); m <= 0 || c <= m {
			s.maxFuel.Store(c)
			p.cap = c
		}
	}
	s.allowance.Store(p)
}

// AllowanceScope is the allowance the request entered, or "".
func AllowanceScope(ctx context.Context) string {
	if s := budgetFromCtx(ctx); s != nil {
		if p := s.allowance.Load(); p != nil {
			return p.name
		}
	}
	return ""
}

// AllowanceFromEnvelope reads the entered allowance's name from an
// envelope's `_txc.allowance`, for the usage lines built from a payload.
func AllowanceFromEnvelope(raw string) string {
	return gjson.Get(raw, "_txc.allowance.name").String()
}
