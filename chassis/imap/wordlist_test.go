package imap

import (
	"strings"
	"testing"
)

func TestWordListIsBIP39(t *testing.T) {
	w := wordList()
	if len(w) != 2048 {
		t.Fatalf("word list has %d entries, want 2048", len(w))
	}
	seen := make(map[string]bool, len(w))
	for _, x := range w {
		if x == "" || strings.ToLower(x) != x || strings.ContainsAny(x, " -\t") {
			t.Fatalf("bad word %q", x)
		}
		if seen[x] {
			t.Fatalf("duplicate word %q", x)
		}
		seen[x] = true
	}
	if w[0] != "abandon" || w[2047] != "zoo" {
		t.Fatalf("list bounds: first=%q last=%q", w[0], w[2047])
	}
}

func TestGenerateWordPassword(t *testing.T) {
	inList := make(map[string]bool, 2048)
	for _, x := range wordList() {
		inList[x] = true
	}
	for _, n := range []int{0, 4, 5, 6} {
		p := GenerateWordPassword(n)
		parts := strings.Split(p, "-")
		want := n
		if n < 1 {
			want = WordPasswordDefaultWords
		}
		if len(parts) != want {
			t.Fatalf("n=%d: %q has %d words, want %d", n, p, len(parts), want)
		}
		for _, w := range parts {
			if !inList[w] {
				t.Fatalf("n=%d: word %q not in the list", n, w)
			}
		}
	}
	// Two draws differ (55 bits: a collision here means the RNG is broken).
	if a, b := GenerateWordPassword(5), GenerateWordPassword(5); a == b {
		t.Fatalf("two generated passwords are identical: %q", a)
	}
	// The default meets the op's minimum length by a wide margin.
	if len(GenerateWordPassword(0)) < 8 {
		t.Fatal("default word password shorter than the 8-char minimum")
	}
}
