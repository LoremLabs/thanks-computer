package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// calendarAccount creates or updates a calendar account for the pinned
// tenant. Result at `into`: {username, created, password?, rotated?} —
// password only when generated (create, explicit "", or `rotate`); rotated
// only when an existing account's password was regenerated. Pass the
// password the IMAP account got (`password = ._imapacct.password`) and the
// two heads share one credential.
func calendarAccount(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	username, domain, err := parseUsername(gjson.GetBytes(meta, "username").String())
	if err != nil {
		return calendarErr(into, "txco_calendar_invalid_arg", err.Error()), nil
	}
	owned, err := d.domainOwned(ctx, tenant, domain)
	if err != nil {
		return calendarErr(into, "txco_calendar_store", err.Error()), nil
	}
	if !owned {
		return calendarErr(into, "txco_calendar_domain_not_owned",
			fmt.Sprintf("domain %q is not a verified hostname or delegated zone of this tenant", domain)), nil
	}
	pwr, pcode, pmsg := resolveAccountPassword(meta, func() (bool, error) {
		_, exists, gerr := d.store.GetAccount(ctx, username)
		return exists, gerr
	})
	if pcode != "" {
		return calendarErr(into, "txco_calendar_"+pcode, pmsg), nil
	}
	status := gjson.GetBytes(meta, "status").String()
	var policy json.RawMessage
	if p := gjson.GetBytes(meta, "policy"); p.Exists() {
		if !p.IsObject() {
			return calendarErr(into, "txco_calendar_invalid_arg", "`policy` must be an object of verb → deny|local|observe|stack"), nil
		}
		if err := chcal.ValidatePolicy(json.RawMessage(p.Raw)); err != nil {
			return calendarErr(into, "txco_calendar_invalid_arg", err.Error()), nil
		}
		policy = json.RawMessage(p.Raw)
	}
	created, err := d.store.UpsertAccount(ctx, tenant, username, pwr.hash, status, policy)
	if err != nil {
		code := "txco_calendar_store"
		if errors.Is(err, chcal.ErrUsernameTaken) {
			code = "txco_calendar_username_taken"
		}
		return calendarErr(into, code, err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".username", username)
	out.Set(into+".created", created)
	out.Set(into+".principal", principalPath(d.prefix, username))
	if pwr.generated != "" {
		out.Set(into+".password", pwr.generated)
	}
	if pwr.rotated {
		out.Set(into+".rotated", true)
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

var colorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

// calendarCalendar ensures (creates or updates), removes, or re-keys the
// feed of one calendar. Result: {id, name, path, display_name, description,
// color, timezone, policy, feed, sync_token, created, feed_token?,
// feed_path?, removed?}. `feed_token` appears only when minted — `rotate`,
// or `ensure` on a calendar that had none — and only this once.
func calendarCalendar(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := calendarAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !chcal.ValidCalendarName(name) {
		return calendarErr(into, "txco_calendar_invalid_arg", "`name` must be a URL segment ([A-Za-z0-9._~-], up to 128 chars)"), nil
	}
	if gjson.GetBytes(meta, "remove").Bool() {
		cal, found, err := d.store.GetCalendar(ctx, tenant, acct.Username, name)
		if err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		removed := false
		if found {
			if removed, err = d.store.RemoveCalendar(ctx, cal.ID); err != nil {
				return calendarErr(into, "txco_calendar_store", err.Error()), nil
			}
		}
		out := jsonx.NewObject()
		out.Set(into+".name", name)
		out.Set(into+".removed", removed)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	c := chcal.Calendar{Tenant: tenant, Username: acct.Username, Name: name,
		DisplayName: gjson.GetBytes(meta, "display_name").String(),
		Description: gjson.GetBytes(meta, "description").String(),
		Color:       strings.TrimSpace(gjson.GetBytes(meta, "color").String()),
		SortOrder:   int(gjson.GetBytes(meta, "sort_order").Int()),
		Timezone:    strings.TrimSpace(gjson.GetBytes(meta, "timezone").String()),
	}
	if c.Color != "" && !colorRE.MatchString(c.Color) {
		return calendarErr(into, "txco_calendar_invalid_arg", "`color` must be #rrggbb"), nil
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "Local" {
			return calendarErr(into, "txco_calendar_invalid_arg", fmt.Sprintf("`timezone` %q is not an IANA zone", c.Timezone)), nil
		}
	}
	if p := gjson.GetBytes(meta, "policy"); p.Exists() {
		if !p.IsObject() {
			return calendarErr(into, "txco_calendar_invalid_arg", "`policy` must be an object of verb → deny|local|observe|stack"), nil
		}
		if err := chcal.ValidatePolicy(json.RawMessage(p.Raw)); err != nil {
			return calendarErr(into, "txco_calendar_invalid_arg", err.Error()), nil
		}
		c.Policy = json.RawMessage(p.Raw)
	}
	feed := gjson.GetBytes(meta, "feed").String()
	switch feed {
	case "", "ensure", "rotate", "disable":
	default:
		return calendarErr(into, "txco_calendar_invalid_arg", "`feed` must be ensure, rotate or disable"), nil
	}
	cal, created, err := d.store.EnsureCalendar(ctx, c)
	if err != nil {
		return calendarErr(into, "txco_calendar_store", err.Error()), nil
	}
	token := ""
	switch {
	case feed == "rotate" || (feed == "ensure" && cal.FeedTokenHash == ""):
		t, h, err := newFeedToken()
		if err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		if err := d.store.SetFeedToken(ctx, cal.ID, h); err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		token = t
		cal.FeedTokenHash = h
	case feed == "disable" && cal.FeedTokenHash != "":
		if err := d.store.SetFeedToken(ctx, cal.ID, ""); err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		cal.FeedTokenHash = ""
	}
	out := jsonx.NewObject()
	calendarJSON(out, into, d.prefix, cal)
	out.Set(into+".created", created)
	if token != "" {
		out.Set(into+".feed_token", token)
		out.Set(into+".feed_path", feedPath(d.prefix, token))
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// calendarPut materializes one object, addressed by UID. Either `event{}`
// (rendered by the chassis; `uid` optional, else derived from `name`) or
// `ical` (text; the UID is the bytes' own). `name` is the resource name on
// create; on update the object keeps the name it has. Same content
// (DTSTAMP/SEQUENCE aside) ⇒ noop; changed content from `event{}` gets
// SEQUENCE bumped. Result: {name, path, uid, etag, created, noop, sequence,
// modseq}.
func calendarPut(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := calendarAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	cal, ep, ok := calendarFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	icalW := gjson.GetBytes(meta, "ical")
	evW := gjson.GetBytes(meta, "event")
	if icalW.Exists() == evW.Exists() {
		return calendarErr(into, "txco_calendar_invalid_arg", "give `event{...}` (the chassis renders it) or `ical` (text), not both"), nil
	}
	uidW := strings.TrimSpace(gjson.GetBytes(meta, "uid").String())
	nameW := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if nameW != "" && !chcal.ValidObjectName(nameW) {
		return calendarErr(into, "txco_calendar_invalid_arg", "`name` must be a URL segment ([A-Za-z0-9._~-], up to 255 chars)"), nil
	}
	now := calendarNow(d)

	var res chcal.PutResult
	var err error
	if evW.Exists() {
		if !evW.IsObject() {
			return calendarErr(into, "txco_calendar_invalid_arg", "`event` must be an object"), nil
		}
		ev, perr := chcal.EventFromJSON([]byte(evW.Raw))
		if perr != nil {
			return calendarErr(into, "txco_calendar_invalid_arg", perr.Error()), nil
		}
		if ev.UID == "" {
			ev.UID = uidW
		}
		if uidW != "" && uidW != ev.UID {
			return calendarErr(into, "txco_calendar_invalid_arg", "`uid` disagrees with event.uid"), nil
		}
		if ev.UID == "" && nameW != "" {
			ev.UID = chcal.DefaultUID(nameW, acct.Username)
		}
		if b, rerr := chcal.Render(ev, now); rerr == nil && d.maxBytes > 0 && int64(len(b)) > d.maxBytes {
			return calendarErr(into, "txco_calendar_too_large",
				fmt.Sprintf("object is %d bytes, over calendar-object-max-bytes %d", len(b), d.maxBytes)), nil
		} else if rerr == nil {
			blobChargeBytes(ctx, int64(len(b)), in)
		}
		res, err = d.store.PutEvent(ctx, cal.ID, acct.Username, nameW, ev, now)
	} else {
		raw := []byte(icalW.String())
		if d.maxBytes > 0 && int64(len(raw)) > d.maxBytes {
			return calendarErr(into, "txco_calendar_too_large",
				fmt.Sprintf("object is %d bytes, over calendar-object-max-bytes %d", len(raw), d.maxBytes)), nil
		}
		if uidW != "" {
			if facts, perr := chcal.Parse(raw); perr == nil && facts.UID != uidW {
				return calendarErr(into, "txco_calendar_invalid_arg", "`uid` disagrees with the UID in `ical`"), nil
			}
		}
		blobChargeBytes(ctx, int64(len(raw)), in)
		res, err = d.store.PutICal(ctx, cal.ID, nameW, raw)
	}
	if err != nil {
		switch {
		case errors.Is(err, chcal.ErrInvalidEvent):
			return calendarErr(into, "txco_calendar_invalid_arg", strings.TrimPrefix(err.Error(), chcal.ErrInvalidEvent.Error()+"\n")), nil
		case errors.Is(err, chcal.ErrUIDConflict):
			return calendarErr(into, "txco_calendar_conflict", err.Error()), nil
		case errors.Is(err, chcal.ErrNotFound):
			return calendarErr(into, "txco_calendar_no_calendar", err.Error()), nil
		}
		return calendarErr(into, "txco_calendar_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", res.Name)
	out.Set(into+".path", objectPath(d.prefix, acct.Username, cal.Name, res.Name))
	out.Set(into+".uid", res.UID)
	out.Set(into+".etag", res.ETag)
	out.Set(into+".created", res.Created)
	out.Set(into+".noop", res.Noop)
	out.Set(into+".sequence", res.Sequence)
	out.Set(into+".modseq", res.ModSeq)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// calendarObjectFor resolves `uid` or `name` to a live object.
func calendarObjectFor(ctx context.Context, d calendarDeps, cal chcal.Calendar, meta []byte, into string) (chcal.Object, event.Payload, bool) {
	uid := strings.TrimSpace(gjson.GetBytes(meta, "uid").String())
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	var o chcal.Object
	var found bool
	var err error
	switch {
	case uid != "":
		o, found, err = d.store.GetObjectByUID(ctx, cal.ID, uid)
	case name != "":
		o, found, err = d.store.GetObject(ctx, cal.ID, name)
	default:
		return chcal.Object{}, calendarErr(into, "txco_calendar_invalid_arg", "give `uid` or `name`"), false
	}
	if err != nil {
		return chcal.Object{}, calendarErr(into, "txco_calendar_store", err.Error()), false
	}
	if !found {
		return chcal.Object{}, calendarErr(into, "txco_calendar_no_object", "no such object in "+cal.Name), false
	}
	return o, event.Payload{}, true
}

// calendarGet returns one object: its row facts, bytes and parsed event.
func calendarGet(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := calendarAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	cal, ep, ok := calendarFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	o, ep, ok := calendarObjectFor(ctx, d, cal, meta, into)
	if !ok {
		return ep, nil
	}
	out := jsonx.NewObject()
	objectJSON(out, into, d.prefix, acct.Username, cal.Name, o)
	out.Set(into+".ical", string(o.ICal))
	if ev, err := chcal.Parse(o.ICal); err == nil {
		if raw, err := json.Marshal(ev); err == nil {
			out.SetRaw(into+".event", string(raw))
		}
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// calendarList lists the account's calendars, or — with `calendar` — one
// calendar's objects after a modseq cursor (`after`, `limit` ≤ 1000).
func calendarList(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := calendarAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	out := jsonx.NewObject()
	if !gjson.GetBytes(meta, "calendar").Exists() {
		cals, err := d.store.ListCalendars(ctx, tenant, acct.Username)
		if err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		for i, c := range cals {
			p := fmt.Sprintf("%s.calendars.%d", into, i)
			calendarJSON(out, p, d.prefix, c)
			objs, err := d.store.ListObjects(ctx, c.ID, chcal.ListOpts{})
			if err != nil {
				return calendarErr(into, "txco_calendar_store", err.Error()), nil
			}
			out.Set(p+".objects", len(objs))
		}
		if len(cals) == 0 {
			out.SetRaw(into+".calendars", "[]")
		}
		out.Set(into+".count", len(cals))
		out.Set(into+".home", homePath(d.prefix, acct.Username))
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	cal, ep, ok := calendarFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	objs, err := d.store.ListObjects(ctx, cal.ID, chcal.ListOpts{SinceModSeq: gjson.GetBytes(meta, "after").Int()})
	if err != nil {
		return calendarErr(into, "txco_calendar_store", err.Error()), nil
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].ModSeq < objs[j].ModSeq })
	next := int64(0)
	if len(objs) > limit {
		objs = objs[:limit]
		next = objs[len(objs)-1].ModSeq
	}
	for i, o := range objs {
		objectJSON(out, fmt.Sprintf("%s.items.%d", into, i), d.prefix, acct.Username, cal.Name, o)
	}
	if len(objs) == 0 {
		out.SetRaw(into+".items", "[]")
	}
	out.Set(into+".count", len(objs))
	out.Set(into+".next", next)
	out.Set(into+".sync_token", cal.SyncToken)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// calendarDelete tombstones one object by `uid` or `name`. Result:
// {deleted, name, uid}; an absent object is `deleted: false`, not an error.
func calendarDelete(ctx context.Context, d calendarDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := calendarPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	acct, ep, ok := calendarAccountFor(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	cal, ep, ok := calendarFor(ctx, d, acct, meta, into)
	if !ok {
		return ep, nil
	}
	uid := strings.TrimSpace(gjson.GetBytes(meta, "uid").String())
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if uid == "" && name == "" {
		return calendarErr(into, "txco_calendar_invalid_arg", "give `uid` or `name`"), nil
	}
	if name == "" {
		o, found, err := d.store.GetObjectByUID(ctx, cal.ID, uid)
		if err != nil {
			return calendarErr(into, "txco_calendar_store", err.Error()), nil
		}
		if !found {
			out := jsonx.NewObject()
			out.Set(into+".deleted", false)
			out.Set(into+".uid", uid)
			return event.Payload{Raw: out.String(), Type: event.JSON}, nil
		}
		name, uid = o.Name, o.UID
	}
	_, found, err := d.store.DeleteObject(ctx, cal.ID, name)
	if err != nil {
		return calendarErr(into, "txco_calendar_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".deleted", found)
	out.Set(into+".name", name)
	if uid != "" {
		out.Set(into+".uid", uid)
	}
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}
