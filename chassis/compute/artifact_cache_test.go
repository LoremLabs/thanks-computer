package compute

import "testing"

func art(n int) Artifact { return Artifact{Wasm: make([]byte, n)} }

// TestArtifactCacheEvictsLeastRecentlyUsed: over budget, the entry used
// longest ago goes first, and the byte count follows.
func TestArtifactCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := newArtifactCache(10)
	a, b, d := Ref{"sha256", "a"}, Ref{"sha256", "b"}, Ref{"sha256", "d"}
	c.put(a, art(4))
	c.put(b, art(4))
	if _, ok := c.get(a); !ok { // a is now the most recently used
		t.Fatal("a missing")
	}
	c.put(d, art(4))
	if _, ok := c.get(b); ok {
		t.Fatal("b kept; the least recently used entry should have gone")
	}
	for _, r := range []Ref{a, d} {
		if _, ok := c.get(r); !ok {
			t.Fatalf("%s evicted, want kept", r.Digest)
		}
	}
	if c.size != 8 {
		t.Fatalf("size = %d, want 8", c.size)
	}
}

// TestArtifactCacheSkipsOversize: an artifact bigger than the whole budget
// isn't kept and evicts nothing.
func TestArtifactCacheSkipsOversize(t *testing.T) {
	c := newArtifactCache(10)
	a, big := Ref{"sha256", "a"}, Ref{"sha256", "big"}
	c.put(a, art(4))
	c.put(big, art(11))
	if _, ok := c.get(big); ok {
		t.Fatal("oversize artifact kept")
	}
	if _, ok := c.get(a); !ok || c.size != 4 {
		t.Fatalf("a evicted or size wrong (size = %d)", c.size)
	}
}
