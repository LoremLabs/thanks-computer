package search

import (
	"encoding/json"
	"strings"
)

// Limits of the stack-facing API. They are public limits, not engine limits:
// every backend enforces the same ones by calling the Validate functions, so a
// request that works on a laptop works hosted.
const (
	MaxTextBytes       = 64 << 10
	MaxShortFieldBytes = 1 << 10 // title, heading, name
	MaxEntities        = 64
	MaxEntityBytes     = 256
	MaxMetadataBytes   = 16 << 10
	MaxMetadataKeys    = 64
	MaxIDBytes         = 512
	MaxItemsPerUpsert  = 500
	MaxIDsPerDelete    = 500
	MaxQueryBytes      = 8 << 10
	MaxNameBytes       = 256 // a collection name
	DefaultLimit       = 10
	MaxLimit           = 100
)

// ValidateCollectionName rejects a name a backend could not address.
func ValidateCollectionName(name string) error {
	switch {
	case name == "":
		return &InvalidArgError{Reason: "collection name required"}
	case len(name) > MaxNameBytes:
		return &TooLargeError{What: "collection name bytes", Got: len(name), Limit: MaxNameBytes}
	case strings.ContainsRune(name, 0):
		return &InvalidArgError{Reason: "collection name must not contain NUL"}
	}
	return nil
}

// ValidateItems checks one upsert: its size, and each item's id and fields.
// A repeated id inside one upsert is refused, because "which one wins" would
// differ between backends.
func ValidateItems(items []Item) error {
	if len(items) == 0 {
		return &InvalidArgError{Reason: "no items to upsert"}
	}
	if len(items) > MaxItemsPerUpsert {
		return &TooLargeError{What: "items per upsert", Got: len(items), Limit: MaxItemsPerUpsert}
	}
	seen := make(map[string]struct{}, len(items))
	for _, it := range items {
		if err := validateID(it.ID); err != nil {
			return err
		}
		if _, dup := seen[it.ID]; dup {
			return &InvalidArgError{Reason: "item id " + quote(it.ID) + " appears twice in one upsert"}
		}
		seen[it.ID] = struct{}{}
		if len(it.Text) > MaxTextBytes {
			return &TooLargeError{What: "text bytes of item " + quote(it.ID), Got: len(it.Text), Limit: MaxTextBytes}
		}
		for _, f := range []struct{ name, val string }{{"title", it.Title}, {"heading", it.Heading}, {"name", it.Name}} {
			if len(f.val) > MaxShortFieldBytes {
				return &TooLargeError{What: f.name + " bytes of item " + quote(it.ID), Got: len(f.val), Limit: MaxShortFieldBytes}
			}
		}
		if len(it.Entities) > MaxEntities {
			return &TooLargeError{What: "entities of item " + quote(it.ID), Got: len(it.Entities), Limit: MaxEntities}
		}
		for _, e := range it.Entities {
			if len(e) > MaxEntityBytes {
				return &TooLargeError{What: "entity bytes of item " + quote(it.ID), Got: len(e), Limit: MaxEntityBytes}
			}
		}
		if err := validateMetadata("metadata of item "+quote(it.ID), it.Metadata); err != nil {
			return err
		}
	}
	return nil
}

// ValidateQuery checks a query string and clamps its limit: zero or less takes
// DefaultLimit, and anything above MaxLimit is MaxLimit.
func ValidateQuery(q string, limit int) (int, error) {
	if len(q) > MaxQueryBytes {
		return 0, &TooLargeError{What: "query bytes", Got: len(q), Limit: MaxQueryBytes}
	}
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit = MaxLimit
	}
	return limit, nil
}

// ValidateSelector checks that a Delete names records exactly one way.
func ValidateSelector(sel Selector) error {
	byID, byFilter := len(sel.IDs) > 0, len(sel.Filter.Conditions) > 0
	switch {
	case byID && byFilter:
		return &InvalidArgError{Reason: "a delete takes `ids` or `filter`, not both"}
	case !byID && !byFilter:
		return &InvalidArgError{Reason: "a delete needs `ids`, or a `filter` with at least one condition"}
	case len(sel.IDs) > MaxIDsPerDelete:
		return &TooLargeError{What: "ids per delete", Got: len(sel.IDs), Limit: MaxIDsPerDelete}
	}
	for _, id := range sel.IDs {
		if err := validateID(id); err != nil {
			return err
		}
	}
	return nil
}

// ValidateUpdate checks that an Update is conditional and changes something.
func ValidateUpdate(filter Filter, ch Change) error {
	if len(filter.Conditions) == 0 {
		return &InvalidArgError{Reason: "an update needs a `filter` with at least one condition"}
	}
	if ch.Empty() {
		return &InvalidArgError{Reason: "an update needs `merge` or `fields`"}
	}
	if err := validateMetadata("merge", ch.Merge); err != nil {
		return err
	}
	if ch.Fields != nil {
		for _, f := range []struct {
			name string
			val  *string
		}{{"name", ch.Fields.Name}, {"title", ch.Fields.Title}, {"heading", ch.Fields.Heading}} {
			if f.val != nil && len(*f.val) > MaxShortFieldBytes {
				return &TooLargeError{What: "fields." + f.name + " bytes", Got: len(*f.val), Limit: MaxShortFieldBytes}
			}
		}
	}
	return nil
}

func validateID(id string) error {
	switch {
	case id == "":
		return &InvalidArgError{Reason: "item id required"}
	case len(id) > MaxIDBytes:
		return &TooLargeError{What: "id bytes", Got: len(id), Limit: MaxIDBytes}
	case strings.ContainsRune(id, 0):
		return &InvalidArgError{Reason: "item id must not contain NUL"}
	}
	return nil
}

func validateMetadata(what string, m map[string]any) error {
	if len(m) == 0 {
		return nil
	}
	if len(m) > MaxMetadataKeys {
		return &TooLargeError{What: "keys in " + what, Got: len(m), Limit: MaxMetadataKeys}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return &InvalidArgError{Reason: what + " is not JSON: " + err.Error()}
	}
	if len(raw) > MaxMetadataBytes {
		return &TooLargeError{What: "bytes of " + what, Got: len(raw), Limit: MaxMetadataBytes}
	}
	return nil
}

func quote(s string) string {
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return `"` + s + `"`
}
