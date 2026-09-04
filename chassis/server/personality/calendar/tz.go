package calendar

import "time"

func loadLocation(id string) (*time.Location, error) {
	if id == "" || id == "Local" {
		return nil, errUnknownZone
	}
	return time.LoadLocation(id)
}

type zoneErr string

func (e zoneErr) Error() string { return string(e) }

const errUnknownZone = zoneErr("unknown time zone")
