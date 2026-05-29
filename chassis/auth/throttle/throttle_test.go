package throttle

import (
	"fmt"
	"testing"
	"time"
)

// TestAllowsUnderLimit — N attempts in a window all return ok=true.
// The (N+1)th returns ok=false with a positive Retry-After.
func TestAllowsUnderLimit(t *testing.T) {
	tr := New(3, time.Second)
	for i := 0; i < 3; i++ {
		ok, retry := tr.Allow("ip-a")
		if !ok || retry != 0 {
			t.Fatalf("attempt %d: got ok=%v retry=%v, want ok=true retry=0", i+1, ok, retry)
		}
	}
	ok, retry := tr.Allow("ip-a")
	if ok {
		t.Errorf("4th attempt should be blocked")
	}
	if retry <= 0 || retry > time.Second {
		t.Errorf("retry should be in (0, window]; got %v", retry)
	}
}

// TestWindowResets — after the window elapses, the bucket clears and
// the next attempt succeeds. Uses a 50ms window so the test is fast
// without being flaky.
func TestWindowResets(t *testing.T) {
	tr := New(1, 50*time.Millisecond)
	if ok, _ := tr.Allow("ip-a"); !ok {
		t.Fatalf("1st should pass")
	}
	if ok, _ := tr.Allow("ip-a"); ok {
		t.Fatalf("2nd should be blocked while window active")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := tr.Allow("ip-a"); !ok {
		t.Errorf("after window, attempt should pass")
	}
}

// TestDifferentKeysIndependent — exhausting ip-A's budget doesn't
// touch ip-B's. The point of keying by IP.
func TestDifferentKeysIndependent(t *testing.T) {
	tr := New(2, time.Second)
	for i := 0; i < 2; i++ {
		if ok, _ := tr.Allow("ip-a"); !ok {
			t.Fatalf("ip-a attempt %d should pass", i+1)
		}
	}
	if ok, _ := tr.Allow("ip-a"); ok {
		t.Fatalf("ip-a should be blocked")
	}
	for i := 0; i < 2; i++ {
		if ok, _ := tr.Allow("ip-b"); !ok {
			t.Errorf("ip-b attempt %d should pass (independent bucket)", i+1)
		}
	}
}

// TestZeroLimitDisables — New(0, …) returns a permissive throttle. No
// bucket allocations happen either, since state never touches the map.
func TestZeroLimitDisables(t *testing.T) {
	tr := New(0, time.Second)
	for i := 0; i < 100; i++ {
		if ok, retry := tr.Allow("ip-a"); !ok || retry != 0 {
			t.Fatalf("disabled throttle should always allow; got ok=%v retry=%v", ok, retry)
		}
	}
	if tr.Size() != 0 {
		t.Errorf("disabled throttle should leave bucket map empty; got Size()=%d", tr.Size())
	}
}

// TestLazyCleanup — buckets older than the window get evicted on
// subsequent Allow calls. We seed N stale buckets, then poke one
// fresh bucket and verify the map shrinks to ~1 entry.
func TestLazyCleanup(t *testing.T) {
	tr := New(5, 20*time.Millisecond)
	for i := 0; i < 50; i++ {
		tr.Allow(fmt.Sprintf("ephemeral-%d", i))
	}
	if tr.Size() != 50 {
		t.Fatalf("setup: expected 50 buckets, got %d", tr.Size())
	}
	// Wait past 2× window so every prior bucket is GC-eligible.
	time.Sleep(50 * time.Millisecond)
	tr.Allow("fresh") // triggers gcLocked
	if got := tr.Size(); got != 1 {
		t.Errorf("after cleanup: want 1 bucket, got %d", got)
	}
}

// TestNegativeLimitClampsToZero — defensive: a misconfigured negative
// limit shouldn't underflow the comparison. Treated as disabled.
func TestNegativeLimitClampsToZero(t *testing.T) {
	tr := New(-5, time.Second)
	if ok, _ := tr.Allow("ip-a"); !ok {
		t.Errorf("negative limit should disable; got blocked")
	}
}

// TestZeroWindowClampsToOneSecond — a zero/negative window with a
// real limit doesn't create an immortal bucket; constructor floors
// it to 1s.
func TestZeroWindowClampsToOneSecond(t *testing.T) {
	tr := New(1, 0)
	if tr.window != time.Second {
		t.Errorf("zero window: want clamped to 1s, got %v", tr.window)
	}
}
