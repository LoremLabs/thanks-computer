package contacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-vcard"
)

// ProdID identifies chassis-rendered vCards.
const ProdID = "-//loremlabs//txco contacts//EN"

// Name is the structured N property.
type Name struct {
	Family     string `json:"family,omitempty"`
	Given      string `json:"given,omitempty"`
	Additional string `json:"additional,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Suffix     string `json:"suffix,omitempty"`
}

// Typed is one EMAIL or TEL value with its TYPE parameters.
type Typed struct {
	Value string   `json:"value"`
	Type  []string `json:"type,omitempty"`
	Pref  bool     `json:"pref,omitempty"`
}

// Card is the chassis's structured form of one vCard: generic vCard
// vocabulary, no product words. On the way IN (txco://contacts/put WITH
// card, @contacts.res.card) the first block is read; on the way OUT
// (@contacts.card, txco://contacts/get) the facts are filled in as well.
// `kind` is individual (default), group, org or location; a group's
// `members` are the member UIDs as the client wrote them (urn:uuid:…).
type Card struct {
	UID      string   `json:"uid,omitempty"`
	FN       string   `json:"fn,omitempty"`
	Name     *Name    `json:"name,omitempty"`
	Nickname string   `json:"nickname,omitempty"`
	Org      string   `json:"org,omitempty"`
	Title    string   `json:"title,omitempty"`
	Note     string   `json:"note,omitempty"`
	URL      string   `json:"url,omitempty"`
	Birthday string   `json:"birthday,omitempty"`
	Emails   []Typed  `json:"emails,omitempty"`
	Phones   []Typed  `json:"phones,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Members  []string `json:"members,omitempty"`

	// Facts the chassis fills in when parsing; ignored when rendering.
	Version   string   `json:"version,omitempty"`
	Rev       string   `json:"rev,omitempty"`
	Addresses []string `json:"addresses"` // EMAIL values, lowercased, deduped, in order
}

// CardFromJSON reads a Card from its JSON form (the WITH card{} param or a
// stack's @contacts.res.card).
func CardFromJSON(raw []byte) (Card, error) {
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil {
		return Card{}, fmt.Errorf("card: %w", err)
	}
	return c, nil
}

// ---- the line layer ------------------------------------------------------
//
// Our own tokenizer, because go-vcard's decoder does not unescape `\;` and
// its encoder neither folds nor escapes `;` and iterates params as a map:
// what a client wrote must be readable as facts here and re-emitted by the
// library without corruption.

type param struct {
	name   string
	values []string
}

type contentLine struct {
	group  string
	name   string // upper-cased
	params []param
	value  string // raw, still escaped
}

func (l contentLine) param(name string) []string {
	var out []string
	for _, p := range l.params {
		if p.name == name {
			out = append(out, p.values...)
		}
	}
	return out
}

// unfold splits bytes into logical lines: BOM stripped, CRLF/CR/LF
// accepted, continuation lines (leading SP/HTAB) joined, blank lines
// dropped.
func unfold(b []byte) []string {
	s := strings.TrimPrefix(string(b), "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += line[1:]
			continue
		}
		out = append(out, line)
	}
	return out
}

func parseLine(s string) (contentLine, error) {
	var cl contentLine
	i := 0
	for i < len(s) && s[i] != ';' && s[i] != ':' {
		i++
	}
	if i == 0 || i >= len(s) {
		return cl, fmt.Errorf("malformed content line %q", s)
	}
	nameTok := s[:i]
	if dot := strings.LastIndex(nameTok, "."); dot >= 0 {
		cl.group = nameTok[:dot]
		nameTok = nameTok[dot+1:]
	}
	cl.name = strings.ToUpper(strings.TrimSpace(nameTok))
	if cl.name == "" {
		return cl, fmt.Errorf("malformed content line %q", s)
	}
	for i < len(s) && s[i] == ';' {
		i++
		j := i
		for j < len(s) && s[j] != '=' && s[j] != ';' && s[j] != ':' {
			j++
		}
		if j >= len(s) {
			return cl, fmt.Errorf("malformed parameter in %q", s)
		}
		pname := strings.ToUpper(strings.TrimSpace(s[i:j]))
		if s[j] != '=' {
			// A bare parameter (vCard 2.1 style `TEL;HOME:`): read it as TYPE.
			cl.params = append(cl.params, param{name: "TYPE", values: []string{pname}})
			i = j
			continue
		}
		k := j + 1
		var vals []string
		var cur strings.Builder
		inQuote := false
		for k < len(s) {
			c := s[k]
			if inQuote {
				if c == '"' {
					inQuote = false
				} else {
					cur.WriteByte(c)
				}
				k++
				continue
			}
			if c == '"' {
				inQuote = true
				k++
				continue
			}
			if c == ',' {
				vals = append(vals, cur.String())
				cur.Reset()
				k++
				continue
			}
			if c == ';' || c == ':' {
				break
			}
			cur.WriteByte(c)
			k++
		}
		if inQuote || k >= len(s) {
			return cl, fmt.Errorf("malformed parameter in %q", s)
		}
		vals = append(vals, cur.String())
		cl.params = append(cl.params, param{name: pname, values: vals})
		i = k
	}
	if i >= len(s) || s[i] != ':' {
		return cl, fmt.Errorf("malformed content line %q", s)
	}
	cl.value = s[i+1:]
	return cl, nil
}

// unescapeText applies RFC 6350 §3.4: \\ \; \, \n \N.
func unescapeText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		switch n := s[i+1]; n {
		case '\\', ';', ',':
			b.WriteByte(n)
			i++
		case 'n', 'N':
			b.WriteByte('\n')
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// splitUnescaped splits a compound/list value on sep, honouring escapes.
func splitUnescaped(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			cur.WriteByte(c)
			cur.WriteByte(s[i+1])
			i++
			continue
		}
		if c == sep {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// tokenize validates the envelope (exactly one BEGIN:VCARD … END:VCARD) and
// returns the content lines between them. Malformed lines are skipped, as
// every client library does.
func tokenize(b []byte) ([]contentLine, error) {
	lines := unfold(b)
	if len(lines) == 0 {
		return nil, errors.New("vcard: empty")
	}
	var out []contentLine
	began, ended := false, false
	for _, raw := range lines {
		cl, err := parseLine(raw)
		if err != nil {
			continue
		}
		if ended {
			return nil, errors.New("vcard: content after END:VCARD (one vCard per object)")
		}
		switch cl.name {
		case "BEGIN":
			if !strings.EqualFold(strings.TrimSpace(cl.value), "VCARD") {
				continue
			}
			if began {
				return nil, errors.New("vcard: nested BEGIN:VCARD (one vCard per object)")
			}
			began = true
			continue
		case "END":
			if strings.EqualFold(strings.TrimSpace(cl.value), "VCARD") {
				if !began {
					return nil, errors.New("vcard: END:VCARD before BEGIN:VCARD")
				}
				ended = true
			}
			continue
		}
		if !began {
			return nil, errors.New("vcard: no BEGIN:VCARD")
		}
		out = append(out, cl)
	}
	if !began {
		return nil, errors.New("vcard: no BEGIN:VCARD")
	}
	if !ended {
		return nil, errors.New("vcard: no END:VCARD")
	}
	return out, nil
}

var versions = map[string]bool{"3.0": true, "4.0": true}

// ErrUnsupportedVersion is a vCard whose VERSION is not 3.0 or 4.0 (the
// CardDAV `supported-address-data` precondition).
var ErrUnsupportedVersion = errors.New("vcard: VERSION is not 3.0 or 4.0")

// Parse reads the facts of a vCard: exactly one VCARD with a VERSION of 3.0
// or 4.0 and a UID. The bytes need not be normalized.
func Parse(b []byte) (Card, error) {
	lines, err := tokenize(b)
	if err != nil {
		return Card{}, err
	}
	c := Card{Kind: "individual", Addresses: []string{}}
	seen := map[string]bool{}
	for _, l := range lines {
		text := func() string { return strings.TrimSpace(unescapeText(l.value)) }
		switch l.name {
		case "VERSION":
			c.Version = strings.TrimSpace(l.value)
		case "UID":
			c.UID = text()
		case "FN":
			if c.FN == "" {
				c.FN = text()
			}
		case "N":
			if c.Name == nil {
				parts := splitUnescaped(l.value, ';')
				for len(parts) < 5 {
					parts = append(parts, "")
				}
				c.Name = &Name{Family: unescapeText(parts[0]), Given: unescapeText(parts[1]), Additional: unescapeText(parts[2]),
					Prefix: unescapeText(parts[3]), Suffix: unescapeText(parts[4])}
			}
		case "NICKNAME":
			if c.Nickname == "" {
				c.Nickname = strings.TrimSpace(unescapeText(splitUnescaped(l.value, ',')[0]))
			}
		case "ORG":
			if c.Org == "" {
				c.Org = strings.TrimSpace(unescapeText(splitUnescaped(l.value, ';')[0]))
			}
		case "TITLE":
			if c.Title == "" {
				c.Title = text()
			}
		case "NOTE":
			if c.Note == "" {
				c.Note = unescapeText(l.value)
			}
		case "URL":
			if c.URL == "" {
				c.URL = strings.TrimSpace(l.value)
			}
		case "BDAY":
			if c.Birthday == "" {
				c.Birthday = strings.TrimSpace(l.value)
			}
		case "EMAIL":
			v := strings.TrimPrefix(text(), "mailto:")
			if v == "" {
				continue
			}
			t, pref := typesOf(l)
			c.Emails = append(c.Emails, Typed{Value: v, Type: t, Pref: pref})
			lc := strings.ToLower(v)
			if !seen[lc] {
				seen[lc] = true
				c.Addresses = append(c.Addresses, lc)
			}
		case "TEL":
			v := strings.TrimPrefix(text(), "tel:")
			if v == "" {
				continue
			}
			t, pref := typesOf(l)
			c.Phones = append(c.Phones, Typed{Value: v, Type: t, Pref: pref})
		case "KIND", "X-ADDRESSBOOKSERVER-KIND":
			if k := strings.ToLower(text()); k != "" {
				c.Kind = k
			}
		case "MEMBER", "X-ADDRESSBOOKSERVER-MEMBER":
			if v := text(); v != "" {
				c.Members = append(c.Members, v)
			}
		case "REV":
			c.Rev = parseRev(strings.TrimSpace(l.value))
		}
	}
	if c.Version == "" {
		return Card{}, errors.New("vcard: no VERSION")
	}
	if !versions[c.Version] {
		return Card{}, fmt.Errorf("%w (%q)", ErrUnsupportedVersion, c.Version)
	}
	if c.UID == "" {
		return Card{}, errors.New("vcard: no UID")
	}
	return c, nil
}

// typesOf reads TYPE/PREF params: lower-cased types without `pref`, and
// whether the field is preferred (PREF=… or TYPE=pref, Apple's 3.0 idiom).
func typesOf(l contentLine) ([]string, bool) {
	var types []string
	pref := len(l.param("PREF")) > 0
	for _, t := range l.param("TYPE") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if t == "pref" {
			pref = true
			continue
		}
		types = append(types, t)
	}
	return types, pref
}

func parseRev(v string) string {
	for _, f := range []string{"20060102T150405Z", time.RFC3339, "20060102T150405", "2006-01-02T15:04:05", "20060102", "2006-01-02"} {
		if t, err := time.Parse(f, v); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// ToLibrary re-reads the bytes as a go-vcard Card for go-webdav's listing
// and query paths (which encode it with go-vcard's encoder). Values are
// unescaped the way that encoder expects to re-escape them; an escaped
// `\;` becomes a bare `;` (that encoder never escapes semicolons).
func ToLibrary(b []byte) (vcard.Card, error) {
	lines, err := tokenize(b)
	if err != nil {
		return nil, err
	}
	card := vcard.Card{}
	for _, l := range lines {
		f := &vcard.Field{Value: unescapeText(l.value), Params: vcard.Params{}, Group: l.group}
		for _, p := range l.params {
			f.Params[p.name] = append(f.Params[p.name], p.values...)
		}
		card.Add(l.name, f)
	}
	if card.Get(vcard.FieldVersion) == nil {
		card.SetValue(vcard.FieldVersion, "3.0")
	}
	return card, nil
}

// Normalize is all a client's bytes get before storage: BOM dropped, line
// endings CRLF, a trailing CRLF.
func Normalize(b []byte) []byte {
	s := strings.TrimPrefix(string(b), "\uFEFF")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimRight(s, "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	return []byte(s + "\r\n")
}

// ---- rendering -----------------------------------------------------------

var (
	emailRE = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)
	bdayRE  = regexp.MustCompile(`^(\d{4})-?(\d{2})-?(\d{2})$`)
	kinds   = map[string]bool{"individual": true, "group": true, "org": true, "location": true}
)

// escapeText applies RFC 6350 §3.4 to a text value (or one component of a
// compound value).
func escapeText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\r", `\n`, "\n", `\n`)
	return r.Replace(s)
}

// fold breaks a content line at 75 octets on rune boundaries (RFC 6350
// §3.2), continuation lines starting with one space.
func fold(line string) string {
	const width = 75
	if len(line) <= width {
		return line
	}
	var b strings.Builder
	limit := width
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 {
			cut = limit
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
		limit = width - 1
	}
	b.WriteString(line)
	return b.String()
}

// Render encodes c as a vCard 3.0 (the RFC 6352 baseline every contacts
// app reads): properties in a fixed order, text escaped, lines folded,
// CRLF. Deterministic apart from REV, which the store's no-op rule
// ignores.
func Render(c Card, now time.Time) ([]byte, error) {
	uid := strings.TrimSpace(c.UID)
	if uid == "" {
		return nil, errors.New("card: uid is required")
	}
	fn := strings.TrimSpace(c.FN)
	if fn == "" && len(c.Emails) > 0 {
		fn = strings.TrimSpace(c.Emails[0].Value)
	}
	if fn == "" {
		return nil, errors.New("card: fn (or an email) is required")
	}
	kind := strings.ToLower(strings.TrimSpace(c.Kind))
	if kind == "" {
		kind = "individual"
	}
	if !kinds[kind] {
		return nil, fmt.Errorf("card.kind: %q is not individual, group, org or location", c.Kind)
	}
	var lines []string
	add := func(name, value string) { lines = append(lines, fold(name+":"+value)) }
	add("BEGIN", "VCARD")
	add("VERSION", "3.0")
	add("PRODID", ProdID)
	add("UID", escapeText(uid))
	if n := c.Name; n != nil {
		add("N", strings.Join([]string{escapeText(n.Family), escapeText(n.Given), escapeText(n.Additional), escapeText(n.Prefix), escapeText(n.Suffix)}, ";"))
	} else {
		// vCard 3.0 requires N; without a structured name the display name
		// takes the family slot so a contacts app never shows "No Name".
		add("N", escapeText(fn)+";;;;")
	}
	add("FN", escapeText(fn))
	if v := strings.TrimSpace(c.Nickname); v != "" {
		add("NICKNAME", escapeText(v))
	}
	if v := strings.TrimSpace(c.Org); v != "" {
		add("ORG", escapeText(v))
	}
	if v := strings.TrimSpace(c.Title); v != "" {
		add("TITLE", escapeText(v))
	}
	for i, e := range c.Emails {
		v := strings.TrimSpace(e.Value)
		if !emailRE.MatchString(v) {
			return nil, fmt.Errorf("card.emails[%d]: %q is not an email address", i, e.Value)
		}
		add("EMAIL;TYPE="+strings.Join(typeList("INTERNET", e.Type, e.Pref), ","), escapeText(v))
	}
	for i, p := range c.Phones {
		v := strings.TrimSpace(p.Value)
		if v == "" {
			return nil, fmt.Errorf("card.phones[%d]: empty", i)
		}
		name := "TEL"
		if t := typeList("", p.Type, p.Pref); len(t) > 0 {
			name += ";TYPE=" + strings.Join(t, ",")
		}
		add(name, escapeText(v))
	}
	if v := strings.TrimSpace(c.URL); v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" {
			return nil, fmt.Errorf("card.url: %q is not an absolute URL", c.URL)
		}
		add("URL", v)
	}
	if v := strings.TrimSpace(c.Birthday); v != "" {
		m := bdayRE.FindStringSubmatch(v)
		if m == nil {
			return nil, fmt.Errorf("card.birthday: %q is not YYYY-MM-DD", c.Birthday)
		}
		add("BDAY", m[1]+"-"+m[2]+"-"+m[3])
	}
	if v := c.Note; strings.TrimSpace(v) != "" {
		add("NOTE", escapeText(v))
	}
	if kind == "group" {
		add("X-ADDRESSBOOKSERVER-KIND", "group")
		for _, m := range c.Members {
			if m = strings.TrimSpace(m); m != "" {
				add("X-ADDRESSBOOKSERVER-MEMBER", escapeText(m))
			}
		}
	}
	add("REV", now.UTC().Format("20060102T150405Z"))
	add("END", "VCARD")
	return []byte(strings.Join(lines, "\r\n") + "\r\n"), nil
}

// typeList builds a TYPE value list: `first` (when given) then the caller's
// types upper-cased and de-duplicated, then PREF when preferred.
func typeList(first string, types []string, pref bool) []string {
	var out []string
	seen := map[string]bool{}
	push := func(t string) {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t == "" || t == "PREF" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	if first != "" {
		push(first)
	}
	for _, t := range types {
		push(t)
	}
	if pref {
		out = append(out, "PREF")
	}
	return out
}
