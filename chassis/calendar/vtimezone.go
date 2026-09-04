package calendar

import (
	"fmt"
	"sort"
	"time"

	"github.com/emersion/go-ical"
)

// VTimezone builds a VTIMEZONE for loc from Go's tzdata by listing the
// zone's transitions as explicit STANDARD/DAYLIGHT observances (no RRULE):
// valid RFC 5545, correct across DST, and simple. The window is
// [Jan 1 of last year, Jan 1 three years out) anchored to now's year, so
// the component changes once a year rather than on every render.
func VTimezone(loc *time.Location, now time.Time) *ical.Component {
	comp := ical.NewComponent(ical.CompTimezone)
	comp.Props.SetText(ical.PropTimezoneID, loc.String())
	from := time.Date(now.Year()-1, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(now.Year()+3, 1, 1, 0, 0, 0, 0, time.UTC)

	type observance struct {
		at       time.Time // the transition instant
		fromOff  int
		toOff    int
		name     string
		daylight bool
	}
	var obs []observance
	name0, off0 := from.In(loc).Zone()
	obs = append(obs, observance{at: from, fromOff: off0, toOff: off0, name: name0})
	prevOff := off0
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		name, off := t.In(loc).Zone()
		if off == prevOff {
			continue
		}
		// Bisect the hour for the exact second of the transition.
		lo, hi := t.Add(-time.Hour), t
		for hi.Sub(lo) > time.Second {
			mid := lo.Add(hi.Sub(lo) / 2)
			if _, o := mid.In(loc).Zone(); o == prevOff {
				lo = mid
			} else {
				hi = mid
			}
		}
		obs = append(obs, observance{at: hi, fromOff: prevOff, toOff: off, name: name, daylight: off > prevOff})
		prevOff = off
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].at.Before(obs[j].at) })
	for _, o := range obs {
		kind := ical.CompTimezoneStandard
		if o.daylight {
			kind = ical.CompTimezoneDaylight
		}
		c := ical.NewComponent(kind)
		// DTSTART is the onset in the local time of the PRIOR offset
		// (RFC 5545 §3.6.5), floating.
		local := o.at.In(time.FixedZone("", o.fromOff))
		p := ical.NewProp(ical.PropDateTimeStart)
		p.Value = local.Format("20060102T150405")
		c.Props.Set(p)
		// UTC-OFFSET values: set raw (SetText would tag them VALUE=TEXT).
		c.Props.Set(&ical.Prop{Name: ical.PropTimezoneOffsetFrom, Params: ical.Params{}, Value: fmtOffset(o.fromOff)})
		c.Props.Set(&ical.Prop{Name: ical.PropTimezoneOffsetTo, Params: ical.Params{}, Value: fmtOffset(o.toOff)})
		if o.name != "" {
			c.Props.SetText(ical.PropTimezoneName, o.name)
		}
		comp.Children = append(comp.Children, c)
	}
	return comp
}

func fmtOffset(sec int) string {
	sign := "+"
	if sec < 0 {
		sign = "-"
		sec = -sec
	}
	h, m := sec/3600, (sec%3600)/60
	return fmt.Sprintf("%s%02d%02d", sign, h, m)
}
