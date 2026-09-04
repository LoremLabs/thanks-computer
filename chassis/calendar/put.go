package calendar

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
)

var objectNameSan = regexp.MustCompile(`[^A-Za-z0-9._~-]+`)

// DefaultObjectName derives a resource name from a UID's local part.
func DefaultObjectName(uid string) string {
	local := uid
	if at := strings.Index(uid, "@"); at > 0 {
		local = uid[:at]
	}
	local = strings.Trim(objectNameSan.ReplaceAllString(local, "-"), "-.")
	if local == "" {
		local = "event"
	}
	if len(local) > 200 {
		local = local[:200]
	}
	return local + ".ics"
}

// DefaultUID is the deterministic UID for a materialized object:
// <name without .ics>.<local>@<domain> — stable across re-materializations,
// unique per persona. Stacks that spell a UID out (OnePony's projection)
// must derive the same value.
func DefaultUID(name, username string) string {
	base := strings.TrimSuffix(strings.TrimSpace(name), ".ics")
	base = strings.Trim(objectNameSan.ReplaceAllString(base, "-"), "-.")
	local, domain := username, ""
	if at := strings.LastIndex(username, "@"); at > 0 {
		local, domain = username[:at], username[at+1:]
	}
	if domain == "" {
		return base + "." + local
	}
	return base + "." + local + "@" + domain
}

// ErrInvalidEvent wraps a validation failure of the event or its bytes, so
// callers can answer "invalid argument" rather than "store failure".
var ErrInvalidEvent = errors.New("calendar: invalid event")

func invalid(err error) error { return errors.Join(ErrInvalidEvent, err) }

// PutEvent materializes ev into the calendar, addressed by UID: the chassis
// renders it, an existing object with that UID is updated in place under
// its own resource name (name is used only on create; "" derives one from
// the UID), unchanged content is a no-op, and changed content gets a
// SEQUENCE above the stored one. ev.UID "" derives DefaultUID(name,
// username). The one write path txco://calendar/put and the CALENDARS/
// seed share.
func (s *Store) PutEvent(ctx context.Context, calID, username, name string, ev Event, now time.Time) (PutResult, error) {
	name = strings.TrimSpace(name)
	if name != "" && !ValidObjectName(name) {
		return PutResult{}, invalid(errors.New("resource name is not a URL segment ([A-Za-z0-9._~-], up to 255 chars)"))
	}
	if strings.TrimSpace(ev.UID) == "" {
		if name == "" {
			return PutResult{}, invalid(errors.New("give a uid or a name (the uid is derived from the name)"))
		}
		ev.UID = DefaultUID(name, username)
	}
	existing, found, err := s.GetObjectByUID(ctx, calID, ev.UID)
	if err != nil {
		return PutResult{}, err
	}
	if found && ev.Sequence < existing.Sequence {
		ev.Sequence = existing.Sequence
	}
	bytes, err := Render(ev, now)
	if err != nil {
		return PutResult{}, invalid(err)
	}
	if found && !SameContent(existing.ICal, bytes) {
		ev.Sequence = existing.Sequence + 1
		if bytes, err = Render(ev, now); err != nil {
			return PutResult{}, invalid(err)
		}
	}
	return s.putBytes(ctx, calID, name, bytes)
}

// PutICal materializes iCalendar text (canonicalized) into the calendar,
// addressed by the bytes' own UID.
func (s *Store) PutICal(ctx context.Context, calID, name string, ical []byte) (PutResult, error) {
	name = strings.TrimSpace(name)
	if name != "" && !ValidObjectName(name) {
		return PutResult{}, invalid(errors.New("resource name is not a URL segment ([A-Za-z0-9._~-], up to 255 chars)"))
	}
	bytes, err := Canonical(ical)
	if err != nil {
		return PutResult{}, invalid(err)
	}
	return s.putBytes(ctx, calID, name, bytes)
}

func (s *Store) putBytes(ctx context.Context, calID, name string, bytes []byte) (PutResult, error) {
	facts, err := Parse(bytes)
	if err != nil {
		return PutResult{}, invalid(err)
	}
	if name == "" {
		name = DefaultObjectName(facts.UID)
	}
	return s.PutObject(ctx, calID, Object{
		Name: name, UID: facts.UID, Component: facts.Component, ICal: bytes, Summary: facts.Summary,
		DTStartUTC: facts.StartUTC, DTEndUTC: facts.EndUTC, Recurs: facts.Recurs, Sequence: facts.Sequence,
	}, PutOpts{ByUID: true})
}
