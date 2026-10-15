package drive_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/drive"
)

func TestLinksValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links drive.Links
		want  string // "" ok, else a substring of the error
	}{
		{"none", nil, ""},
		{"two", drive.Links{"teams/red": "red", "teams/blue": "blue"}, ""},
		{"top-level", drive.Links{"red": "red"}, ""},
		{"root", drive.Links{"": "red"}, "root"},
		{"dotdot", drive.Links{"a/../b": "red"}, "segment"},
		{"unnormalized", drive.Links{"/teams/red/": "red"}, "write it as"},
		{"bad name", drive.Links{"teams/red": "no/slash"}, "not a collection name"},
		{"nested", drive.Links{"teams": "all", "teams/red": "red"}, "inside link"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.links.Validate()
			if tc.want == "" && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("Validate = %v, want an error with %q", err, tc.want)
			}
		})
	}
	if _, err := drive.ParseLinks(`{"x": "has space"}`); err == nil {
		t.Fatal("ParseLinks accepted a bad name")
	}
	if l, err := drive.ParseLinks(""); err != nil || l != nil {
		t.Fatalf("ParseLinks(\"\") = %v, %v", l, err)
	}
}

func TestLinksResolve(t *testing.T) {
	l := drive.Links{"teams/red": "red", "teams/blue": "blue", "solo": "s"}
	for _, tc := range []struct {
		p, at, name, inner string
		ok                 bool
	}{
		{"teams/red", "teams/red", "red", "", true},
		{"teams/red/Input/a.pdf", "teams/red", "red", "Input/a.pdf", true},
		{"teams/redder", "", "", "", false},
		{"teams", "", "", "", false},
		{"solo/x", "solo", "s", "x", true},
		{"", "", "", "", false},
		{"Wiki/index.md", "", "", "", false},
	} {
		at, name, inner, ok := l.Resolve(tc.p)
		if at != tc.at || name != tc.name || inner != tc.inner || ok != tc.ok {
			t.Errorf("Resolve(%q) = %q %q %q %v, want %q %q %q %v", tc.p, at, name, inner, ok, tc.at, tc.name, tc.inner, tc.ok)
		}
	}
	if got := l.Under(""); !reflect.DeepEqual(got, map[string]bool{"teams": false, "solo": true}) {
		t.Errorf("Under(\"\") = %v", got)
	}
	if got := l.Under("teams"); !reflect.DeepEqual(got, map[string]bool{"red": true, "blue": true}) {
		t.Errorf("Under(teams) = %v", got)
	}
	if !l.Holds("teams") || !l.Holds("teams/red") || l.Holds("teams/red/Input") || l.Holds("other") {
		t.Error("Holds: wrong")
	}
	if !l.Leads("teams") || l.Leads("teams/red") || !l.Leads("") || l.Leads("solo") {
		t.Error("Leads: wrong")
	}
}

func TestSetCollectionLinks(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	got, err := s.SetCollectionLinks(ctx, c.Tenant, c.Name, drive.Links{"teams/red": "red"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Links["teams/red"] != "red" {
		t.Fatalf("links = %v", got.Links)
	}
	re, _, _ := s.GetCollection(ctx, c.Tenant, c.Name)
	if re.Links["teams/red"] != "red" {
		t.Fatalf("reread links = %v", re.Links)
	}
	if _, err := s.SetCollectionLinks(ctx, c.Tenant, c.Name, drive.Links{"a": "x", "a/b": "y"}); err == nil {
		t.Fatal("nested links accepted")
	}
	cleared, err := s.SetCollectionLinks(ctx, c.Tenant, c.Name, nil)
	if err != nil || len(cleared.Links) != 0 {
		t.Fatalf("clear: %v %v", cleared.Links, err)
	}
	if _, err := s.SetCollectionLinks(ctx, c.Tenant, "missing", drive.Links{"a": "x"}); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("missing collection: %v", err)
	}
}

func TestCopyAcross(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	a := newColl(t, s)
	b, _, err := s.EnsureCollection(ctx, a.Tenant, "other")
	if err != nil {
		t.Fatal(err)
	}
	s.Mkdir(ctx, a.ID, "dir")
	put(t, s, a.ID, "dir/f.txt", "hello")
	put(t, s, a.ID, "g.txt", "gee")

	// A file, into another collection: the source untouched, the copy b's.
	res, err := s.CopyAcross(ctx, a.ID, "g.txt", b.ID, "g.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, s, b.ID, "g.txt"); got != "gee" {
		t.Fatalf("copied = %q", got)
	}
	if got, _ := read(t, s, a.ID, "g.txt"); got != "gee" {
		t.Fatalf("source = %q", got)
	}
	if res.ResourceID == "" {
		t.Fatal("no id")
	}
	// A directory, with what's below it; to a path below the source's own
	// path (no cycle across two collections).
	s.Mkdir(ctx, b.ID, "dir")
	if _, err := s.CopyAcross(ctx, a.ID, "dir", b.ID, "dir/dir", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, s, b.ID, "dir/dir/f.txt"); got != "hello" {
		t.Fatalf("copied dir file = %q", got)
	}
	bc, _, _ := s.GetCollection(ctx, b.Tenant, b.Name)
	if bc.ResourceCount != 4 { // g.txt, dir, dir/dir, dir/dir/f.txt
		t.Fatalf("destination count = %d", bc.ResourceCount)
	}
	// Exists without overwrite; overwrite replaces.
	if _, err := s.CopyAcross(ctx, a.ID, "g.txt", b.ID, "g.txt", false); !errors.Is(err, drive.ErrExists) {
		t.Fatalf("exists: %v", err)
	}
	put(t, s, a.ID, "g.txt", "two")
	if _, err := s.CopyAcross(ctx, a.ID, "g.txt", b.ID, "g.txt", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, s, b.ID, "g.txt"); got != "two" {
		t.Fatalf("overwritten = %q", got)
	}
	// Another tenant's collection is not one to copy into.
	z, _, _ := s.EnsureCollection(ctx, "tnt_z", "zed")
	if _, err := s.CopyAcross(ctx, a.ID, "g.txt", z.ID, "g.txt", false); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("cross-tenant: %v", err)
	}
	// The root is never copied.
	if _, err := s.CopyAcross(ctx, a.ID, "", b.ID, "x", false); !errors.Is(err, drive.ErrBadPath) {
		t.Fatalf("root: %v", err)
	}
}
