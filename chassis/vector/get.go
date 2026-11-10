package vector

import "fmt"

// get.go holds the parts of Store.Get that do not depend on a backend: the
// checks on a selector, and the assembly of an answer from the rows a
// backend read. Every backend runs its query between the two, so the rules
// (the bounds, the order, Missing, More) are one implementation, and the
// conformance suite holds each backend to them.

// NormalizeGet checks a Get's selector and paging, and returns the selector
// with duplicate ids removed (first occurrence kept) and the limit a filter
// Get should use. A backend calls it first and queries with what it returns.
func NormalizeGet(sel Selector, after string, limit int) (Selector, int, error) {
	hasIDs := len(sel.IDs) > 0
	hasFilter := len(sel.Filter.Conditions) > 0
	switch {
	case hasIDs && hasFilter:
		return Selector{}, 0, &InvalidArgError{Reason: "get names items by ids or by filter, not both"}
	case !hasIDs && !hasFilter:
		return Selector{}, 0, &InvalidArgError{Reason: "get needs ids or a filter with at least one condition"}
	}
	if hasIDs {
		if len(sel.IDs) > MaxGetIDs {
			return Selector{}, 0, &InvalidArgError{Reason: fmt.Sprintf("get names at most %d ids (got %d)", MaxGetIDs, len(sel.IDs))}
		}
		if after != "" || limit != 0 {
			return Selector{}, 0, &InvalidArgError{Reason: "`after` and `limit` page a filter get; a get by ids takes neither"}
		}
		seen := make(map[string]bool, len(sel.IDs))
		ids := make([]string, 0, len(sel.IDs))
		for _, id := range sel.IDs {
			if id == "" {
				return Selector{}, 0, &InvalidArgError{Reason: "get ids must be non-empty"}
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		return Selector{IDs: ids}, 0, nil
	}
	if limit <= 0 {
		limit = DefaultGetLimit
	}
	if limit > MaxGetLimit {
		return Selector{}, 0, &InvalidArgError{Reason: fmt.Sprintf("get limit is at most %d (got %d)", MaxGetLimit, limit)}
	}
	return Selector{Filter: sel.Filter}, limit, nil
}

// ResultByIDs assembles a get by ids: the items in the order asked, an id the
// collection does not hold listed in Missing, and the answer stopped at the
// text bound. asked is NormalizeGet's ids; found is what the backend read,
// by id.
func ResultByIDs(asked []string, found map[string]Record) GetResult {
	res := GetResult{Items: []Record{}}
	used := 0
	for _, id := range asked {
		r, ok := found[id]
		if !ok {
			res.Missing = append(res.Missing, id)
			continue
		}
		if res.More {
			continue // past the bound: not returned, not missing
		}
		if len(res.Items) > 0 && used+len(r.Text) > MaxGetTextBytes {
			res.More = true
			continue
		}
		used += len(r.Text)
		res.Items = append(res.Items, r)
	}
	return res
}

// ResultByFilter assembles a get by filter from rows in ascending id order.
// The backend reads limit+1 rows, so a row past the limit means the filter
// admits more; the answer also stops at the text bound.
func ResultByFilter(rows []Record, limit int) GetResult {
	res := GetResult{Items: []Record{}}
	used := 0
	for i, r := range rows {
		if i >= limit {
			res.More = true
			break
		}
		if len(res.Items) > 0 && used+len(r.Text) > MaxGetTextBytes {
			res.More = true
			break
		}
		used += len(r.Text)
		res.Items = append(res.Items, r)
	}
	return res
}
