package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
)

// backend adapts the store to go-webdav's caldav.Backend. The principal
// comes from the request context (handler.go authenticated it); every path
// is checked against it, so one account never sees another's tree.
type backend struct{ c *Controller }

func feedTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func httpErr(code int, format string, a ...any) error {
	return webdav.NewHTTPError(code, fmt.Errorf(format, a...))
}

func (b *backend) principal(ctx context.Context) (principal, error) {
	pr, ok := principalFrom(ctx)
	if !ok {
		return principal{}, httpErr(http.StatusUnauthorized, "no principal")
	}
	return pr, nil
}

func (b *backend) principalPath(pr principal) string { return b.c.prefix + "/" + pr.username + "/" }
func (b *backend) homePath(pr principal) string      { return b.principalPath(pr) + "calendars/" }
func (b *backend) calPath(pr principal, name string) string {
	return b.homePath(pr) + name + "/"
}

// parse resolves a request path to (calendar name, resource name). depth
// is 3 for a calendar, 4 for an object; anything else is refused.
func (b *backend) parse(ctx context.Context, path string) (pr principal, calName, resource string, depth int, err error) {
	pr, err = b.principal(ctx)
	if err != nil {
		return
	}
	parts := b.c.pathParts(path)
	depth = len(parts)
	if depth < 1 || parts[0] != pr.username {
		return pr, "", "", depth, httpErr(http.StatusForbidden, "not your principal")
	}
	if depth >= 2 && parts[1] != "calendars" {
		return pr, "", "", depth, httpErr(http.StatusNotFound, "no such collection")
	}
	if depth >= 3 {
		calName = parts[2]
	}
	if depth >= 4 {
		resource = parts[3]
	}
	if depth > 4 {
		return pr, "", "", depth, httpErr(http.StatusNotFound, "no such resource")
	}
	return
}

func (b *backend) calendarAt(ctx context.Context, path string, wantDepth int) (principal, chcal.Calendar, string, error) {
	pr, calName, resource, depth, err := b.parse(ctx, path)
	if err != nil {
		return pr, chcal.Calendar{}, "", err
	}
	if depth != wantDepth {
		return pr, chcal.Calendar{}, "", httpErr(http.StatusNotFound, "no such resource")
	}
	cal, found, err := b.c.store.GetCalendar(ctx, pr.tenant, pr.username, calName)
	if err != nil {
		return pr, chcal.Calendar{}, "", httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return pr, chcal.Calendar{}, "", httpErr(http.StatusNotFound, "no such calendar")
	}
	return pr, cal, resource, nil
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.principalPath(pr), nil
}

func (b *backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return "", err
	}
	return b.homePath(pr), nil
}

func (b *backend) toCalendar(pr principal, c chcal.Calendar) caldav.Calendar {
	name := c.DisplayName
	if name == "" {
		name = c.Name
	}
	return caldav.Calendar{Path: b.calPath(pr, c.Name), Name: name, Description: c.Description,
		MaxResourceSize: b.c.maxBytes, SupportedComponentSet: []string{ical.CompEvent}}
}

func (b *backend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	pr, err := b.principal(ctx)
	if err != nil {
		return nil, err
	}
	cals, err := b.c.store.ListCalendars(ctx, pr.tenant, pr.username)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	out := make([]caldav.Calendar, 0, len(cals))
	for _, c := range cals {
		out = append(out, b.toCalendar(pr, c))
	}
	return out, nil
}

func (b *backend) GetCalendar(ctx context.Context, path string) (*caldav.Calendar, error) {
	pr, cal, _, err := b.calendarAt(ctx, path, 3)
	if err != nil {
		return nil, err
	}
	c := b.toCalendar(pr, cal)
	return &c, nil
}

// CreateCalendar is reached through the library's extended-MKCOL path
// (handler.go intercepts MKCALENDAR and MKCOL itself); kept for
// completeness with the same policy.
func (b *backend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	pr, calName, _, depth, err := b.parse(ctx, cal.Path)
	if err != nil {
		return err
	}
	if depth != 3 {
		return httpErr(http.StatusForbidden, "calendars live under %s", b.homePath(pr))
	}
	_, status, msg := b.c.createCalendar(ctx, pr, calName, map[string]string{"displayname": cal.Name, "description": cal.Description})
	if status != 0 {
		return httpErr(status, "%s", msg)
	}
	return nil
}

func (b *backend) toObject(pr principal, cal chcal.Calendar, o chcal.Object) (caldav.CalendarObject, error) {
	data, err := chcal.Decode(o.ICal)
	if err != nil {
		return caldav.CalendarObject{}, httpErr(http.StatusInternalServerError, "stored object unreadable: %v", err)
	}
	return caldav.CalendarObject{Path: b.calPath(pr, cal.Name) + o.Name, ModTime: o.UpdatedAt, ContentLength: o.Size, ETag: o.ETag, Data: data}, nil
}

func (b *backend) GetCalendarObject(ctx context.Context, path string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	pr, cal, resource, err := b.calendarAt(ctx, path, 4)
	if err != nil {
		return nil, err
	}
	o, found, err := b.c.store.GetObject(ctx, cal.ID, resource)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return nil, httpErr(http.StatusNotFound, "no such object")
	}
	co, err := b.toObject(pr, cal, o)
	if err != nil {
		return nil, err
	}
	return &co, nil
}

func (b *backend) ListCalendarObjects(ctx context.Context, path string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	pr, cal, _, err := b.calendarAt(ctx, path, 3)
	if err != nil {
		return nil, err
	}
	objs, err := b.c.store.ListObjects(ctx, cal.ID, chcal.ListOpts{})
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, o := range objs {
		co, err := b.toObject(pr, cal, o)
		if err != nil {
			continue
		}
		out = append(out, co)
	}
	return out, nil
}

// QueryCalendarObjects: the store's coarse range prefilter (recurring rows
// always pass), then the library's Match, which expands RRULE/EXDATE and
// applies prop/text filters. A Match error keeps the object rather than
// failing the report.
func (b *backend) QueryCalendarObjects(ctx context.Context, path string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	pr, cal, _, err := b.calendarAt(ctx, path, 3)
	if err != nil {
		return nil, err
	}
	var start, end time.Time
	if query != nil {
		start, end = timeRangeOf(query.CompFilter)
	}
	objs, err := b.c.store.ObjectsInRange(ctx, cal.ID, start, end)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, o := range objs {
		co, err := b.toObject(pr, cal, o)
		if err != nil {
			continue
		}
		if query != nil {
			if ok, merr := caldav.Match(query.CompFilter, &co); merr == nil && !ok {
				continue
			}
		}
		out = append(out, co)
	}
	return out, nil
}

// timeRangeOf finds the VEVENT-level time-range of a calendar-query.
func timeRangeOf(f caldav.CompFilter) (time.Time, time.Time) {
	if !f.Start.IsZero() || !f.End.IsZero() {
		return f.Start, f.End
	}
	for _, c := range f.Comps {
		if s, e := timeRangeOf(c); !s.IsZero() || !e.IsZero() {
			return s, e
		}
	}
	return time.Time{}, time.Time{}
}

func condETag(m webdav.ConditionalMatch) (string, error) {
	if !m.IsSet() {
		return "", nil
	}
	if m.IsWildcard() {
		return "*", nil
	}
	return m.ETag()
}

func (b *backend) PutCalendarObject(ctx context.Context, path string, data *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	pr, cal, resource, err := b.calendarAt(ctx, path, 4)
	if err != nil {
		return nil, err
	}
	if !chcal.ValidObjectName(resource) {
		return nil, httpErr(http.StatusForbidden, "resource name is not a URL segment")
	}
	bytes, err := chcal.Encode(data)
	if err != nil {
		return nil, httpErr(http.StatusBadRequest, "valid-calendar-data: %v", err)
	}
	facts, err := chcal.ParseCalendar(data)
	if err != nil {
		return nil, httpErr(http.StatusBadRequest, "valid-calendar-object-resource: %v", err)
	}
	if int64(len(bytes)) > b.c.maxBytes {
		return nil, httpErr(http.StatusRequestEntityTooLarge, "max-resource-size: object over %d bytes", b.c.maxBytes)
	}
	existing, exists, err := b.c.store.GetObject(ctx, cal.ID, resource)
	if err != nil {
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opPut, calendar: refOf(cal),
		object: &objRef{Name: resource, UID: facts.UID, Component: facts.Component, Size: int64(len(bytes)), Exists: exists},
		ical:   bytes, event: &facts, clientIP: pr.clientIP}
	if exists {
		m.object.PriorETag = existing.ETag
		if pe, err := chcal.Parse(existing.ICal); err == nil {
			m.prior = &pe
		}
	}
	if status, msg := b.c.gate(&cal, &pr.acct, chcal.VerbPut, &m); status != 0 {
		return nil, httpErr(status, "%s", msg)
	}
	if m.rewrite != nil {
		// The stack's canonical version replaces the client's bytes; the
		// UID stays the client's so its resource keeps its identity.
		switch {
		case m.rewrite.event != nil:
			ev := *m.rewrite.event
			ev.UID = facts.UID
			if ev.Sequence < facts.Sequence {
				ev.Sequence = facts.Sequence
			}
			// A rewrite that replaces an existing object is a change the
			// client did not author: give it a SEQUENCE above the stored one
			// unless the client already did (RFC 5545 §3.8.7.4).
			if exists && ev.Sequence <= existing.Sequence {
				ev.Sequence = existing.Sequence + 1
			}
			if bytes, err = chcal.Render(ev, b.c.now()); err != nil {
				return nil, httpErr(http.StatusServiceUnavailable, "stack rewrite did not render: %v", err)
			}
		case len(m.rewrite.ical) > 0:
			if bytes, err = chcal.Canonical(m.rewrite.ical); err != nil {
				return nil, httpErr(http.StatusServiceUnavailable, "stack rewrite did not parse: %v", err)
			}
		}
		if facts, err = chcal.Parse(bytes); err != nil {
			return nil, httpErr(http.StatusServiceUnavailable, "stack rewrite did not parse: %v", err)
		}
	}
	var putOpts chcal.PutOpts
	if opts != nil {
		if putOpts.IfMatch, err = condETag(opts.IfMatch); err != nil {
			return nil, httpErr(http.StatusBadRequest, "If-Match: %v", err)
		}
		if putOpts.IfNoneMatch, err = condETag(opts.IfNoneMatch); err != nil {
			return nil, httpErr(http.StatusBadRequest, "If-None-Match: %v", err)
		}
	}
	res, err := b.c.store.PutObject(ctx, cal.ID, chcal.Object{
		Name: resource, UID: facts.UID, Component: facts.Component, ICal: bytes, Summary: facts.Summary,
		DTStartUTC: facts.StartUTC, DTEndUTC: facts.EndUTC, Recurs: facts.Recurs, Sequence: facts.Sequence,
	}, putOpts)
	switch {
	case errors.Is(err, chcal.ErrPrecondition):
		return nil, httpErr(http.StatusPreconditionFailed, "precondition failed")
	case errors.Is(err, chcal.ErrUIDConflict):
		return nil, httpErr(http.StatusConflict, "no-uid-conflict: the UID names another resource")
	case errors.Is(err, chcal.ErrNotFound):
		return nil, httpErr(http.StatusNotFound, "no such calendar")
	case err != nil:
		return nil, httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	m.object.ETag = res.ETag
	m.object.Size = int64(len(bytes))
	m.ical = bytes
	m.event = &facts
	b.c.after(&cal, &pr.acct, chcal.VerbPut, m)
	return &caldav.CalendarObject{Path: path, ETag: res.ETag, ModTime: b.c.now(), ContentLength: int64(len(bytes))}, nil
}

func (b *backend) DeleteCalendarObject(ctx context.Context, path string) error {
	pr, cal, resource, err := b.calendarAt(ctx, path, 4)
	if err != nil {
		return err
	}
	existing, found, err := b.c.store.GetObject(ctx, cal.ID, resource)
	if err != nil {
		return httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	if !found {
		return httpErr(http.StatusNotFound, "no such object")
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opDelete, calendar: refOf(cal),
		object:   &objRef{Name: resource, UID: existing.UID, ETag: existing.ETag, PriorETag: existing.ETag, Component: existing.Component, Size: existing.Size, Exists: true},
		clientIP: pr.clientIP}
	if pe, err := chcal.Parse(existing.ICal); err == nil {
		m.prior = &pe
	}
	if status, msg := b.c.gate(&cal, &pr.acct, chcal.VerbDelete, &m); status != 0 {
		return httpErr(status, "%s", msg)
	}
	if _, _, err := b.c.store.DeleteObject(ctx, cal.ID, resource); err != nil {
		return httpErr(http.StatusServiceUnavailable, "store: %v", err)
	}
	b.c.after(&cal, &pr.acct, chcal.VerbDelete, m)
	return nil
}

// silence the unused-import guard when strings is only used in tests
var _ = strings.TrimSpace
