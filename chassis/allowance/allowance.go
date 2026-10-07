// Package allowance is a tenant's own fuel budgets inside its tenant budget.
//
// An allowance is a named budget a tenant defines for part of its work — one
// per customer, team, agent or feature, whatever the tenant's own unit is —
// as `{name, fuel, per}`: so much fuel per UTC hour, day or month. A request
// enters an allowance (`txco://allowance/enter`), and from then on its fuel
// is charged to that allowance as well as to the tenant. When the window's
// fuel is spent the chassis refuses the request through the shared admission
// marker (`429 allowance_exhausted`, Retry-After = the window's end), the
// same path a tenant rate limit takes, so every head renders it in its own
// protocol.
//
// The tenant's own budget is untouched: allowance usage is still the tenant's
// usage, and whichever runs out first refuses.
//
// Definitions and counters live in the tenant's KV under chassis-reserved
// namespaces, so the author-facing txco://kv/* ops can neither read nor reset
// them. Counters are charged from the usage path (Tee), so everything that
// bills a request — its own line, resumed continuation segments, attachment
// heartbeats — fills the allowance it ran under.
package allowance

import (
	"fmt"
	"time"
)

// Period is an allowance's window: fixed UTC hours, days or months.
type Period string

const (
	Hour  Period = "hour"
	Day   Period = "day"
	Month Period = "month"
)

// DefaultPeriod windows the counters of an allowance that was entered but
// never defined (metered, not limited).
const DefaultPeriod = Day

// ParsePeriod accepts "hour" | "day" | "month".
func ParsePeriod(s string) (Period, error) {
	switch p := Period(s); p {
	case Hour, Day, Month:
		return p, nil
	default:
		return "", fmt.Errorf("allowance: per must be hour, day or month, not %q", s)
	}
}

// Window is the window containing t: an id that names it in a counter key,
// and its UTC bounds [Start, End).
func (p Period) Window(t time.Time) (id string, start, end time.Time) {
	t = t.UTC()
	switch p {
	case Hour:
		start = t.Truncate(time.Hour)
		return "h" + start.Format("2006010215"), start, start.Add(time.Hour)
	case Month:
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return "m" + start.Format("200601"), start, start.AddDate(0, 1, 0)
	default:
		start = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		return "d" + start.Format("20060102"), start, start.AddDate(0, 0, 1)
	}
}

// Def is one allowance: Fuel per Per. Fuel is the limit; it must be positive.
type Def struct {
	Name string `json:"name"`
	Fuel int64  `json:"fuel"`
	Per  Period `json:"per"`
}

// Validate checks the name, the limit and the period.
func (d Def) Validate() error {
	if !ValidName(d.Name) {
		return fmt.Errorf("allowance: invalid name %q (lowercase letters, digits, '.', '_', '-'; up to %d; starts with a letter or digit)", d.Name, maxName)
	}
	if d.Fuel <= 0 {
		return fmt.Errorf("allowance: fuel must be positive, not %d", d.Fuel)
	}
	if _, err := ParsePeriod(string(d.Per)); err != nil {
		return err
	}
	return nil
}

const maxName = 64

// ValidName: 1–64 of [a-z0-9._-], starting with a letter or digit. No '/',
// which a KV key segment cannot hold and which is kept for nesting later.
func ValidName(s string) bool {
	if s == "" || len(s) > maxName {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Status is one allowance's current window. Defined is false for a name
// that was never set: such an allowance is metered (Used counts) but not
// limited.
type Status struct {
	Name     string
	Defined  bool
	Fuel     int64 // the limit; 0 when not defined
	Per      Period
	Used     int64
	ResetsAt time.Time
}

// Remaining is the fuel left in the window (never negative); 0 when not
// defined — check Defined first.
func (s Status) Remaining() int64 {
	if !s.Defined || s.Used >= s.Fuel {
		return 0
	}
	return s.Fuel - s.Used
}

// Exhausted reports a defined allowance with nothing left in its window.
func (s Status) Exhausted() bool { return s.Defined && s.Used >= s.Fuel }
