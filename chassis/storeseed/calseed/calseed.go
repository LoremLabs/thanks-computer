// Package calseed is the CALENDARS/ store-seed Materializer: it reconciles
// CALENDARS/<username>/<calendar>.jsonl packs (chassis/storeseed) into the
// calendar store (chassis/calendar) the `calendar` personality serves.
//
// A pack OWNS one calendar of one account (managed scope). It is NDJSON:
//
//	{"calendar":{"display_name":"…","description":"…","color":"#rrggbb","timezone":"Europe/Paris","policy":{…}}}
//	{"name":"welcome.ics","uid":"…","event":{"summary":"…","start":"2026-09-04T18:00:00","tzid":"Europe/Paris","duration":"PT1H"}}
//	{"name":"notice.ics","ical":"BEGIN:VCALENDAR\r\n…"}
//
// The optional `calendar` line (at most one, anywhere) sets the calendar's
// display fields; every other line is one object — `event{}` (the chassis
// renders it; `uid` defaults to <name>.<local>@<domain>) or `ical` text.
// Reconcile ensures the calendar, puts every object (unchanged content is a
// no-op, changed content gets a higher SEQUENCE), and deletes every live
// object in the calendar whose UID the pack no longer lists.
//
// The ACCOUNT must already exist — only an op mints a password — and belong
// to the tenant; otherwise the pack is an error (logged, activation
// unaffected, retried next apply). A calendar this materializer CREATES
// gets a policy that denies client `put`/`delete` unless the header says
// otherwise: a client edit in a pack-owned calendar would be undone by the
// next apply — the KV managed-scope hazard in calendar clothing. Keep
// runtime-written calendars (OnePony's `schedule`) out of packs.
package calseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// header is the pack's `calendar` line.
type header struct {
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Color       string          `json:"color"`
	SortOrder   int             `json:"sort_order"`
	Timezone    string          `json:"timezone"`
	Policy      json.RawMessage `json:"policy"`
}

// item is one object line.
type item struct {
	Name  string          `json:"name"`
	UID   string          `json:"uid"`
	Event json.RawMessage `json:"event"`
	ICal  string          `json:"ical"`
}

type line struct {
	Calendar json.RawMessage `json:"calendar"`
	item
}

// Materializer reconciles CALENDARS/ packs into a *calendar.Store.
type Materializer struct {
	store  *chcal.Store
	shared bool
	now    func() time.Time
}

// New builds the calendar Materializer. shared declares whether the store
// backend is fleet-shared (postgres) — reconciled once on the origin — or
// per-node (sqlite). The wiring layer (server.go) knows the backend.
func New(store *chcal.Store, shared bool) *Materializer {
	return &Materializer{store: store, shared: shared, now: func() time.Time { return time.Now().UTC() }}
}

func (m *Materializer) Kind() string { return storeseed.KindCalendar }
func (m *Materializer) Shared() bool { return m.shared }

// Reconcile makes each pack's calendar match the pack. Errors are
// aggregated per pack so one bad pack does not skip the rest.
func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	if m.store == nil {
		return errors.New("calendar store not configured")
	}
	var errs []error
	for _, p := range packs {
		if p.Path == "" {
			continue // EmptyTree marker: single-file kinds keep "pack removed = stop managing"
		}
		if err := m.reconcileOne(ctx, scope, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Path, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Materializer) reconcileOne(ctx context.Context, scope storeseed.Scope, p storeseed.RawPack) error {
	username, calName, ok := storeseed.CalendarPackName(p.Name)
	if !ok || !chcal.ValidCalendarName(calName) {
		return fmt.Errorf("pack name %q is not <username>/<calendar>", p.Name)
	}
	username = chcal.NormalizeUsername(username)
	acct, found, err := m.store.GetAccount(ctx, username)
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}
	if !found {
		return fmt.Errorf("no calendar account %q — provision it with txco://calendar/account first (a pack cannot mint a password)", username)
	}
	if acct.Tenant != scope.Tenant {
		return fmt.Errorf("account %q belongs to another tenant", username)
	}
	hdr, items, err := parsePack(p)
	if err != nil {
		return err
	}
	cal := chcal.Calendar{Tenant: scope.Tenant, Username: username, Name: calName}
	if hdr != nil {
		cal.DisplayName, cal.Description, cal.Color, cal.SortOrder, cal.Timezone = hdr.DisplayName, hdr.Description, hdr.Color, hdr.SortOrder, hdr.Timezone
		if len(hdr.Policy) > 0 {
			if err := chcal.ValidatePolicy(hdr.Policy); err != nil {
				return fmt.Errorf("calendar.policy: %w", err)
			}
			cal.Policy = hdr.Policy
		}
		if cal.Timezone != "" {
			if _, terr := time.LoadLocation(cal.Timezone); terr != nil || cal.Timezone == "Local" {
				return fmt.Errorf("calendar.timezone %q is not an IANA zone", cal.Timezone)
			}
		}
	}
	if _, exists, gerr := m.store.GetCalendar(ctx, scope.Tenant, username, calName); gerr != nil {
		return fmt.Errorf("calendar: %w", gerr)
	} else if !exists && len(cal.Policy) == 0 {
		// A pack-owned calendar is not a client's to edit.
		cal.Policy = json.RawMessage(`{"put":"deny","delete":"deny"}`)
	}
	ensured, _, err := m.store.EnsureCalendar(ctx, cal)
	if err != nil {
		return fmt.Errorf("ensure calendar: %w", err)
	}
	now := m.now()
	keep := map[string]struct{}{}
	var errs []error
	for i, it := range items {
		var res chcal.PutResult
		var perr error
		if it.ICal != "" {
			res, perr = m.store.PutICal(ctx, ensured.ID, it.Name, []byte(it.ICal))
		} else {
			ev, eerr := chcal.EventFromJSON(it.Event)
			if eerr != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", i+1, eerr))
				continue
			}
			if ev.UID == "" {
				ev.UID = it.UID
			}
			res, perr = m.store.PutEvent(ctx, ensured.ID, username, it.Name, ev, now)
		}
		if perr != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, perr))
			continue
		}
		keep[res.UID] = struct{}{}
	}
	// Delete-missing: the pack is the desired state of this calendar.
	live, err := m.store.ListObjects(ctx, ensured.ID, chcal.ListOpts{})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list objects: %w", err))...)
	}
	for _, o := range live {
		if _, ok := keep[o.UID]; ok {
			continue
		}
		if _, _, derr := m.store.DeleteObject(ctx, ensured.ID, o.Name); derr != nil {
			errs = append(errs, fmt.Errorf("delete stale %q: %w", o.Name, derr))
		}
	}
	return errors.Join(errs...)
}

// parsePack decodes the pack: at most one `calendar` header, then object
// lines each carrying `event` or `ical`. Malformed lines fail the pack.
func parsePack(p storeseed.RawPack) (*header, []item, error) {
	var hdr *header
	var items []item
	for i, raw := range p.Lines() {
		var ln line
		if err := json.Unmarshal(raw, &ln); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if len(ln.Calendar) > 0 && string(ln.Calendar) != "null" {
			if hdr != nil {
				return nil, nil, fmt.Errorf("line %d: a second `calendar` header", i+1)
			}
			var h header
			if err := json.Unmarshal(ln.Calendar, &h); err != nil {
				return nil, nil, fmt.Errorf("line %d: calendar: %w", i+1, err)
			}
			hdr = &h
			continue
		}
		hasEvent := len(ln.Event) > 0 && string(ln.Event) != "null"
		hasICal := strings.TrimSpace(ln.ICal) != ""
		if hasEvent == hasICal {
			return nil, nil, fmt.Errorf("line %d: give `event` or `ical`, not both or neither", i+1)
		}
		if hasEvent && !json.Valid(ln.Event) {
			return nil, nil, fmt.Errorf("line %d: event is not valid JSON", i+1)
		}
		if hasEvent && ln.UID == "" && ln.Name == "" {
			return nil, nil, fmt.Errorf("line %d: an event needs a `name` or a `uid`", i+1)
		}
		items = append(items, ln.item)
	}
	return hdr, items, nil
}
