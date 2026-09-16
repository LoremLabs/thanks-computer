package drive

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	long := strings.Repeat("a", 256)
	deep := strings.TrimSuffix(strings.Repeat("d/", 33), "/")
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"/", "", true},
		{"//", "", true},
		{"a", "a", true},
		{"/a/b/", "a/b", true},
		{"a//b", "", false},
		{"./a", "", false},
		{"a/../b", "", false},
		{"a/.", "", false},
		{"...", "...", true}, // three dots is an ordinary name
		{"a\x00b", "", false},
		{"café", "café", true},
		{"\xff", "", false},
		{long, "", false},
		{deep, "", false},
		{"with space/and %/_under", "with space/and %/_under", true},
	} {
		got, err := NormalizePath(tc.in)
		if tc.ok != (err == nil) || got != tc.want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrBadPath) {
			t.Errorf("NormalizePath(%q) error not ErrBadPath: %v", tc.in, err)
		}
	}
	if _, err := NormalizePath(strings.Repeat("ab/", 20) + strings.Repeat("c", 970)); err == nil {
		t.Error("over MaxPathBytes accepted")
	}
}

func TestPathHelpers(t *testing.T) {
	if ParentOf("a/b/c") != "a/b" || ParentOf("a") != "" || ParentOf("") != "" {
		t.Error("ParentOf")
	}
	if BaseOf("a/b/c") != "c" || BaseOf("a") != "a" || BaseOf("") != "" {
		t.Error("BaseOf")
	}
	if Depth("") != 0 || Depth("a") != 1 || Depth("a/b/c") != 3 {
		t.Error("Depth")
	}
	if JoinPath("", "x") != "x" || JoinPath("a", "x") != "a/x" {
		t.Error("JoinPath")
	}
	if !IsInside("a/b", "a") || IsInside("ab", "a") || IsInside("a", "a") || !IsInside("a", "") || IsInside("", "") {
		t.Error("IsInside")
	}
	if LikePrefix("a_b%") != `a\_b\%/%` {
		t.Errorf("LikePrefix = %q", LikePrefix("a_b%"))
	}
	for seg, ok := range map[string]bool{"dr_abc": true, "tnt_x": true, "a.b": true, "..": false, ".": false, "": false, "a/b": false, "a b": false, "é": false} {
		if ValidKeySegment(seg) != ok {
			t.Errorf("ValidKeySegment(%q) = %v", seg, !ok)
		}
	}
}
