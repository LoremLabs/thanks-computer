// Package webfeed is the contract behind txco://web/feed: the request a
// rule may make, the answer it gets back, and the reading of a feed
// document (RSS, Atom or JSON Feed) into that answer. It does no
// networking: the handler in chassis/server fetches through chassis/fetch
// and hands the body here.
//
// A feed is what a site publishes about itself: its newest items, each a
// title, an address, a date and a few lines. Reading it answers "what has
// this site published" without fetching the site's pages, which many
// refuse to a program. Items are external text: the stack that reads
// them decides what they may do.
package webfeed

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// The contract's bounds.
const (
	MaxCount     = 50
	DefaultCount = 20
	// MaxScanItems is how many items are read from one document; any past
	// them are left unread and the answer says it was truncated. A feed
	// holds its newest few dozen.
	MaxScanItems = 500

	MaxTitleBytes   = 1 << 10
	MaxSummaryBytes = 4 << 10
	MaxURLBytes     = 2 << 10
	MaxIDBytes      = 1 << 10
	MaxAuthorBytes  = 256
	MaxAnswerBytes  = 256 << 10
)

// DefaultInto is where the answer lands when a rule names no `into`.
const DefaultInto = "_feed"

// Error codes. The handler prefixes them txco_feed_; the fetcher's own
// codes (timeout, destination_denied, http_error, …) pass through under
// the same prefix.
const (
	CodeInvalidRequest = "invalid_request"
	CodeDisabled       = "disabled"
	CodeRateLimited    = "rate_limited"
	CodeBusy           = "busy"
	CodeNotAFeed       = "not_a_feed"
	CodeInvalidFeed    = "invalid_feed"
	CodeNoTenant       = "no_tenant"
	CodeInternal       = "internal_error"
)

// Request is one validated read.
type Request struct {
	URL   string
	Count int
	// Since keeps only items dated at or after it. An undated item is
	// kept: nothing says it is old.
	Since *time.Time
}

// Item is one entry of the feed.
type Item struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Date is the item's own date (published, else updated), RFC 3339 in
	// UTC, or null.
	Date    *string `json:"date"`
	Summary string  `json:"summary"`
	Author  string  `json:"author,omitempty"`
	// Source is the item URL's host, lower case.
	Source string `json:"source,omitempty"`
}

// Info is what the feed says about itself.
type Info struct {
	Title       string  `json:"title"`
	Link        string  `json:"link,omitempty"`
	Description string  `json:"description,omitempty"`
	Updated     *string `json:"updated"`
}

// Answer is the op's result object on success.
type Answer struct {
	OK       bool   `json:"ok"`
	URL      string `json:"url"`
	FinalURL string `json:"final_url,omitempty"`
	Status   int    `json:"status,omitempty"`
	// Format is rss (RSS 2.0, 0.9x and RSS 1.0), atom or json.
	Format string `json:"format"`
	Feed   Info   `json:"feed"`
	Items  []Item `json:"items"`
	Count  int    `json:"count"`
	// Total is how many items the document holds (up to MaxScanItems),
	// before `since` and `count`.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

// Failure is the op's result object when the feed was not read. The
// body is never in it.
type Failure struct {
	OK          bool   `json:"ok"`
	URL         string `json:"url"`
	FinalURL    string `json:"final_url,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Error       *Error `json:"error"`
}

// Error is the in-band failure. Feeds is set on not_a_feed when the
// address was an HTML page that advertises feeds: the addresses to try.
type Error struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Status  int      `json:"status,omitempty"`
	Feeds   []string `json:"feeds,omitempty"`
}

func (e *Error) Error() string { return "webfeed: " + e.Code + ": " + e.Message }

// Errorf builds an *Error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// opKeys are the request's fields. chassisKeys are WITH keys the chassis
// reads for every op (the list web/search keeps); a rule may set them
// beside the request's. Any other key is refused, so a typo such as
// `sinse` fails instead of being ignored.
var (
	opKeys      = map[string]bool{"url": true, "count": true, "since": true}
	chassisKeys = map[string]bool{
		"into": true, "timeout": true, "mode": true, "redact": true, "omit": true,
		"secrets": true, "grant": true, "debug": true, "comment": true, "note": true,
		"stream": true, "max_duration": true, "continue_after": true,
		"checkpoint": true, "join_at_scope": true,
	}
)

// Parse reads and checks the op's WITH object. The URL itself is checked
// by the handler with the fetcher's policy. An *Error names the first
// field at fault.
func Parse(meta []byte) (Request, *Error) {
	req := Request{Count: DefaultCount}
	if len(meta) > 0 && !gjson.ValidBytes(meta) {
		return req, invalid("the request does not parse")
	}
	var bad *Error
	gjson.ParseBytes(meta).ForEach(func(k, _ gjson.Result) bool {
		key := k.String()
		if !opKeys[key] && !chassisKeys[key] {
			bad = invalid("`%s` is not a field of this operation", key)
			return false
		}
		return true
	})
	if bad != nil {
		return req, bad
	}
	get := func(key string) gjson.Result { return gjson.GetBytes(meta, key) }
	// An optional field set to null is one not given, so a rule may pass
	// `count = @cap.input.count` whether or not the caller named one.
	given := func(key string) (gjson.Result, bool) {
		v := get(key)
		return v, v.Exists() && v.Type != gjson.Null
	}

	u := get("url")
	if !u.Exists() || u.Type == gjson.Null {
		return req, invalid("`url` is required")
	}
	if u.Type != gjson.String {
		return req, invalid("`url` must be a string")
	}
	req.URL = strings.TrimSpace(u.String())
	if req.URL == "" {
		return req, invalid("`url` is empty")
	}
	if v, ok := given("count"); ok {
		n, whole := wholeNumber(v)
		if !whole {
			return req, invalid("`count` must be a whole number")
		}
		if n < 1 || n > MaxCount {
			return req, invalid("`count` must be from 1 to %d", MaxCount)
		}
		req.Count = n
	}
	if v, ok := given("since"); ok {
		if v.Type != gjson.String {
			return req, invalid("`since` must be an RFC 3339 instant or a date")
		}
		t, ok := parseSince(strings.TrimSpace(v.String()))
		if !ok {
			return req, invalid("`since` must be an RFC 3339 instant or a date (YYYY-MM-DD)")
		}
		req.Since = &t
	}
	return req, nil
}

func parseSince(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// wholeNumber reads a JSON number, or a string holding one, as an int.
func wholeNumber(v gjson.Result) (int, bool) {
	switch v.Type {
	case gjson.Number:
		if v.Num != float64(int(v.Num)) {
			return 0, false
		}
		return int(v.Num), true
	case gjson.String:
		s := strings.TrimSpace(v.Str)
		if s == "" || len(s) > 6 {
			return 0, false
		}
		n := 0
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	return 0, false
}

func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// Select applies the request to a document's items: those before `since`
// go; when every remaining item is dated they are put newest first,
// otherwise the document's order stands; the first `count` are kept; and
// each is cut to the contract's caps (a title, summary or author at its
// cap on a character boundary; an id or URL over its cap dropped from the
// item, since a cut one points somewhere else). The answer as a whole
// stays under MaxAnswerBytes by dropping items from the end. Truncated
// reports any cut or drop for size; `count` is a choice, not a cut.
func Select(items []Item, req Request) (out []Item, truncated bool) {
	kept := make([]Item, 0, len(items))
	dated := true
	for _, it := range items {
		if it.Date == nil {
			dated = false
		} else if req.Since != nil {
			if t, err := time.Parse(time.RFC3339, *it.Date); err == nil && t.Before(*req.Since) {
				continue
			}
		}
		kept = append(kept, it)
	}
	if dated && len(kept) > 1 {
		// RFC 3339 in UTC, to the second: the strings order as the instants do.
		sort.SliceStable(kept, func(i, j int) bool { return *kept[i].Date > *kept[j].Date })
	}
	count := req.Count
	if count <= 0 {
		count = DefaultCount
	}
	if len(kept) > count {
		kept = kept[:count]
	}
	out = []Item{}
	total := 0
	for _, it := range kept {
		if len(it.Title) > MaxTitleBytes {
			it.Title, truncated = cut(it.Title, MaxTitleBytes), true
		}
		if len(it.Summary) > MaxSummaryBytes {
			it.Summary, truncated = cut(it.Summary, MaxSummaryBytes), true
		}
		if len(it.Author) > MaxAuthorBytes {
			it.Author, truncated = cut(it.Author, MaxAuthorBytes), true
		}
		if len(it.ID) > MaxIDBytes {
			it.ID, truncated = "", true
		}
		if len(it.URL) > MaxURLBytes {
			it.URL, it.Source, truncated = "", "", true
		}
		size := len(it.ID) + len(it.Title) + len(it.URL) + len(it.Summary) + len(it.Author) + len(it.Source) + 128
		if it.Date != nil {
			size += len(*it.Date)
		}
		if total+size > MaxAnswerBytes {
			truncated = true
			break
		}
		total += size
		out = append(out, it)
	}
	return out, truncated
}

// cut shortens s to at most n bytes on a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
