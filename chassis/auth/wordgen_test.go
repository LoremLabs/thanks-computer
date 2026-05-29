package auth

import (
	"math"
	"strings"
	"testing"
)

// TestEffLongWordsShape — the embedded list must be 7772 entries (EFF
// long minus the four hyphen-containing words) and every entry must
// be a non-empty lowercase ASCII string without an interior hyphen.
// EightWordSecret's invariants depend on these.
func TestEffLongWordsShape(t *testing.T) {
	const want = 7772
	if got := len(effLongWords); got != want {
		t.Fatalf("len(effLongWords) = %d, want %d", got, want)
	}
	seen := make(map[string]struct{}, len(effLongWords))
	for i, w := range effLongWords {
		if w == "" {
			t.Fatalf("effLongWords[%d] is empty", i)
		}
		for _, r := range w {
			if !((r >= 'a' && r <= 'z')) {
				t.Fatalf("effLongWords[%d]=%q has non-lowercase rune %q", i, w, r)
			}
		}
		if _, dup := seen[w]; dup {
			t.Fatalf("effLongWords contains duplicate %q (idx %d)", w, i)
		}
		seen[w] = struct{}{}
	}
}

// TestEightWordSecretShape — the secret is 8 hyphen-separated tokens
// drawn from effLongWords. Anyone who logs or shares the secret can
// rely on a fixed shape.
func TestEightWordSecretShape(t *testing.T) {
	s, err := EightWordSecret()
	if err != nil {
		t.Fatalf("EightWordSecret: %v", err)
	}
	parts := strings.Split(s, "-")
	if len(parts) != 8 {
		t.Fatalf("got %d parts, want 8 (raw=%q)", len(parts), s)
	}
	in := make(map[string]struct{}, len(effLongWords))
	for _, w := range effLongWords {
		in[w] = struct{}{}
	}
	for _, p := range parts {
		if _, ok := in[p]; !ok {
			t.Fatalf("part %q not in effLongWords (raw=%q)", p, s)
		}
	}
}

// TestEightWordSecretDistinct — two consecutive calls must differ.
// Collision probability at ~103 bits is essentially zero.
func TestEightWordSecretDistinct(t *testing.T) {
	a, err := EightWordSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := EightWordSecret()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two consecutive secrets matched: %q", a)
	}
}

// TestEightWordSecretDistribution — across N samples we expect a
// large number of distinct leading words. Threshold catches a stuck
// or strongly biased RNG without being flaky.
func TestEightWordSecretDistribution(t *testing.T) {
	const n = 4096
	leading := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		s, err := EightWordSecret()
		if err != nil {
			t.Fatal(err)
		}
		leading[strings.Split(s, "-")[0]] = struct{}{}
	}
	// With 7772 buckets and 4096 samples, expected distinct ≈
	// 7772 * (1 - (1 - 1/7772)^4096) ≈ 3030. Threshold of 2000 is
	// safely below that.
	if len(leading) < 2000 {
		t.Fatalf("only %d distinct leading words across %d samples; RNG may be biased", len(leading), n)
	}
}

// TestRandIndexUniform — randIndex over a small modulus should be
// roughly uniform. Builds confidence the rejection-sampling math is
// right (in particular that we're not double-counting the top half
// of the uint64 range).
func TestRandIndexUniform(t *testing.T) {
	const n = 7
	const samples = 70_000
	counts := make([]int, n)
	for i := 0; i < samples; i++ {
		idx, err := randIndex(n)
		if err != nil {
			t.Fatal(err)
		}
		counts[idx]++
	}
	expected := float64(samples) / float64(n)
	for i, c := range counts {
		dev := math.Abs(float64(c)-expected) / expected
		if dev > 0.05 {
			t.Errorf("bucket %d count=%d expected≈%.0f deviation=%.2f%% (>5%%)", i, c, expected, dev*100)
		}
	}
}
