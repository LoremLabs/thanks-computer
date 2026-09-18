package authn

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	dbschemas "github.com/loremlabs/thanks-computer/db"
)

func TestParsePrincipal(t *testing.T) {
	for in, kind := range map[string]PrincipalKind{
		"user:usr_7HqZ3kYb9mNpQrSt": KindUser,
		"pony:paris":                "pony",
		"service:billing-sync":      "service",
		"pony:Paris_2@one.pony":     "pony",
	} {
		p, err := ParsePrincipal(in)
		if err != nil || p.ID != in || p.Kind != kind || p.IsZero() {
			t.Errorf("%q → %+v, %v", in, p, err)
		}
	}
	for _, in := range []string{
		"", "paris", ":paris", "pony:", "Pony:paris", "pony:pa ris", "pony:a:b", "pony:-lead",
		"user:paris",    // a user principal is minted, never named
		"user:usr_0OIl", // not base58
		"pony:" + strings.Repeat("x", 129),
	} {
		if p, err := ParsePrincipal(in); err == nil {
			t.Errorf("%q parsed as %+v", in, p)
		}
	}
	u := UserPrincipal("usr_7HqZ3kYb9mNpQrSt")
	if id, ok := u.UserID(); !ok || id != "usr_7HqZ3kYb9mNpQrSt" || u.Name() != id {
		t.Errorf("user principal: %+v id=%q ok=%v", u, id, ok)
	}
	pony, _ := ParsePrincipal("pony:paris")
	if _, ok := pony.UserID(); ok || pony.Name() != "paris" {
		t.Errorf("pony principal: %+v", pony)
	}
}

func TestBaseStack(t *testing.T) {
	for in, want := range map[string]string{
		"web":                "web",
		"web/canary":         "web", // a slot is its base stack
		"web/_mail":          "web", // so is a channel into it
		"hello-world/canary": "hello-world",
		"_imap":              "_imap",
		" /web/canary ":      "web",
		"":                   "",
	} {
		if got := BaseStack(in); got != want {
			t.Errorf("BaseStack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopes(t *testing.T) {
	for _, bad := range []string{
		"", "imap", "imap:login", "imap:*:login:x", "*:*:*", "*:inbox:read", "admin:all",
		"IMAP:*:login", "imap:*:Login", "imap:in box:read", "imap:a*:read", "imap::read", "imap:*:",
	} {
		if sc, err := ParseScope(bad); err == nil {
			t.Errorf("scope %q parsed as %v", bad, sc)
		}
	}
	ss, err := ParseScopes([]string{" imap:*:login ", "drive:dc_7Hq:*", "ipp:front-desk:print", "imap:*:login"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ss.Strings(), " "); got != "drive:dc_7Hq:* imap:*:login ipp:front-desk:print" {
		t.Errorf("normalized = %q", got)
	}
	for _, c := range []struct {
		domain, instance, action string
		want                     bool
	}{
		{"imap", "paris@onepony.com", "login", true},
		{"imap", "paris@onepony.com", "append", false}, // the action is pinned
		{"drive", "dc_7Hq", "write", true},
		{"drive", "dc_other", "write", false}, // the instance is pinned
		{"ipp", "front-desk", "print", true},
		{"calendar", "x", "read", false}, // a head the credential never named
		{"imap", "*", "login", true},     // "any" is covered by a grant of "any"
		{"drive", "*", "write", false},   // …and by nothing narrower
	} {
		if got := ss.Allows(c.domain, c.instance, c.action); got != c.want {
			t.Errorf("Allows(%s:%s:%s) = %v", c.domain, c.instance, c.action, got)
		}
	}
	if round, err := decodeScopes(ss.encode()); err != nil || strings.Join(round.Strings(), " ") != strings.Join(ss.Strings(), " ") {
		t.Errorf("round trip: %v %v", round, err)
	}
	// A corrupt column is an error, never "no restrictions".
	for _, col := range []string{"", "null", "[]", `["*:*:*"]`, `{"a":1}`} {
		if got, err := decodeScopes(col); err == nil {
			t.Errorf("decodeScopes(%q) = %v", col, got)
		}
	}
	var many []string
	for i := 0; i <= MaxScopes; i++ {
		many = append(many, "imap:m"+strings.Repeat("x", i)+":read")
	}
	if _, err := ParseScopes(many); err == nil {
		t.Error("more than MaxScopes accepted")
	}
}

func TestIssuedPasswordCarriesItsShortID(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := newShortID()
		if len(id) != shortIDLen || strings.ContainsAny(id, "aeiouyl01") {
			t.Fatalf("short id %q", id)
		}
		for _, style := range []PasswordStyle{StyleToken, StyleWords, ""} {
			pw, err := issuePassword(style, 0, id)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := ShortIDOf(pw); !ok || got != id {
				t.Fatalf("%s password %q → id %q ok=%v, want %q", style, pw, got, ok, id)
			}
		}
	}
	tok, _ := issuePassword(StyleToken, 0, "k7m2")
	if !strings.HasPrefix(tok, "txc_k7m2_") || len(tok) != len("txc_k7m2_")+tokenSecretLen {
		t.Errorf("token = %q", tok)
	}
	words, _ := issuePassword(StyleWords, 7, "k7m2")
	if parts := strings.Split(words, "-"); len(parts) != 8 || parts[0] != "k7m2" {
		t.Errorf("words = %q", words)
	}
	for _, bad := range []string{
		"", "k7m2", "k7m2-", "txc_k7m2", "txc_k7m2_", "txc__secret",
		"river-galaxy-bamboo-orbit-velvet", // a legacy words password: `river` has vowels
		"abcd-efgh-jkmn-pqrs",              // a legacy token password: `abcd` has a vowel
		"k7m-river-galaxy",                 // id too short
		"K7M2-river-galaxy",                // ids are lowercase
		strings.Repeat("k", 17) + "-river",
	} {
		if id, ok := ShortIDOf(bad); ok {
			t.Errorf("ShortIDOf(%q) = %q", bad, id)
		}
	}
	// A longer id still parses, so the length can grow without stranding
	// issued passwords.
	if id, ok := ShortIDOf("k7m2p9-river-galaxy"); !ok || id != "k7m2p9" {
		t.Errorf("long id: %q %v", id, ok)
	}
	for _, c := range []struct {
		style PasswordStyle
		words int
	}{{"pin", 0}, {StyleWords, 3}, {StyleWords, 13}} {
		if _, err := issuePassword(c.style, c.words, "k7m2"); err == nil {
			t.Errorf("issuePassword(%q, %d) accepted", c.style, c.words)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got, err := NormalizeEmail("  Alice.B+tag@Example.COM "); err != nil || got != "alice.b+tag@example.com" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "alice", "@example.com", "alice@", "a@b@c", "Alice <a@example.com>",
		"a b@example.com", "a@exa mple.com", "a\n@example.com", strings.Repeat("x", 250) + "@example.com"} {
		if got, err := NormalizeEmail(bad); err == nil {
			t.Errorf("NormalizeEmail(%q) = %q", bad, got)
		}
	}
}

// A short-id collision inside a principal must be re-rolled, not surfaced.
func TestIssueCredentialRerollsAShortIDCollision(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "auth.db")+"?mode=rwc&_busy_timeout=15000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body, err := fs.ReadFile(dbschemas.FS, "schema/sqlite/auth/0005_identity.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, registry.SQLite)

	ids := []string{"k7m2", "k7m2", "k7m2", "p9vw"}
	orig := newShortID
	newShortID = func() string { id := ids[0]; ids = ids[1:]; return id }
	defer func() { newShortID = orig }()

	ctx := context.Background()
	pony, _ := ParsePrincipal("pony:paris")
	first, _, err := s.IssueCredential(ctx, "tnt_a", "web", pony, NewCredential{Scopes: []string{"imap:*:*"}})
	if err != nil || first.ShortID != "k7m2" {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, pw, err := s.IssueCredential(ctx, "tnt_a", "web", pony, NewCredential{Scopes: []string{"imap:*:*"}})
	if err != nil || second.ShortID != "p9vw" {
		t.Fatalf("second: %+v %v", second, err)
	}
	if got, ok, err := s.VerifyPassword(ctx, "tnt_a", pony, pw); err != nil || !ok || got.ID != second.ID {
		t.Errorf("verify after a re-roll: %+v ok=%v err=%v", got, ok, err)
	}
	// The same id under ANOTHER principal is no collision at all.
	ids = []string{"k7m2"}
	other, _ := ParsePrincipal("pony:milan")
	if c, _, err := s.IssueCredential(ctx, "tnt_a", "web", other, NewCredential{Scopes: []string{"imap:*:*"}}); err != nil || c.ShortID != "k7m2" {
		t.Errorf("same id, other principal: %+v %v", c, err)
	}
}
