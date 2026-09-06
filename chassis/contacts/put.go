package contacts

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
		local = "card"
	}
	if len(local) > 200 {
		local = local[:200]
	}
	return local + ".vcf"
}

// DefaultUID is the deterministic UID for a materialized object:
// <name without .vcf>.<local>@<domain> — stable across re-materializations,
// unique per persona. Stacks that spell a UID out must derive the same
// value.
func DefaultUID(name, username string) string {
	base := strings.TrimSuffix(strings.TrimSpace(name), ".vcf")
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

// ErrInvalidCard wraps a validation failure of the card or its bytes, so
// callers can answer "invalid argument" rather than "store failure".
var ErrInvalidCard = errors.New("contacts: invalid card")

func invalid(err error) error { return errors.Join(ErrInvalidCard, err) }

var errBadName = errors.New("resource name is not a URL segment ([A-Za-z0-9._~-], up to 255 chars)")

// ObjectFromCard renders c (vCard 3.0) into a storable Object. c.UID ""
// derives DefaultUID(name, username), so name is then required. name ""
// derives DefaultObjectName(uid).
func ObjectFromCard(username, name string, c Card, now time.Time) (Object, error) {
	name = strings.TrimSpace(name)
	if name != "" && !ValidObjectName(name) {
		return Object{}, invalid(errBadName)
	}
	if strings.TrimSpace(c.UID) == "" {
		if name == "" {
			return Object{}, invalid(errors.New("give a uid or a name (the uid is derived from the name)"))
		}
		c.UID = DefaultUID(name, username)
	}
	bytes, err := Render(c, now)
	if err != nil {
		return Object{}, invalid(err)
	}
	return objectOf(name, bytes)
}

// ObjectFromVCard validates raw vCard text and wraps it (normalized) into
// a storable Object addressed by the bytes' own UID.
func ObjectFromVCard(name string, raw []byte) (Object, error) {
	name = strings.TrimSpace(name)
	if name != "" && !ValidObjectName(name) {
		return Object{}, invalid(errBadName)
	}
	return objectOf(name, Normalize(raw))
}

func objectOf(name string, bytes []byte) (Object, error) {
	facts, err := Parse(bytes)
	if err != nil {
		return Object{}, invalid(err)
	}
	if name == "" {
		name = DefaultObjectName(facts.UID)
	}
	return Object{
		Name: name, UID: facts.UID, VCard: bytes, Size: int64(len(bytes)),
		Version: facts.Version, FN: facts.FN, Kind: facts.Kind, Addresses: facts.Addresses,
	}, nil
}

// PutCard materializes c into the address book, addressed by UID: an
// existing object with that UID is updated in place under its own resource
// name (name is used only on create), unchanged content is a no-op. The
// write path txco://contacts/put and the CONTACTS/ seed share.
func (s *Store) PutCard(ctx context.Context, abID, username, name string, c Card, now time.Time) (PutResult, error) {
	o, err := ObjectFromCard(username, name, c, now)
	if err != nil {
		return PutResult{}, err
	}
	return s.PutObject(ctx, abID, o, PutOpts{ByUID: true})
}

// PutVCard materializes raw vCard text into the address book, addressed by
// the bytes' own UID.
func (s *Store) PutVCard(ctx context.Context, abID, name string, raw []byte) (PutResult, error) {
	o, err := ObjectFromVCard(name, raw)
	if err != nil {
		return PutResult{}, err
	}
	return s.PutObject(ctx, abID, o, PutOpts{ByUID: true})
}
