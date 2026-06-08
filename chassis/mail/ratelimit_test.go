package mail

import (
	"testing"
	"time"
)

func TestParseRateRules(t *testing.T) {
	got := parseRateRules("100/2m, 200/4h")
	if len(got) != 2 || got[0].max != 100 || got[0].window != 2*time.Minute ||
		got[1].max != 200 || got[1].window != 4*time.Hour {
		t.Fatalf("parse: %+v", got)
	}
	// Malformed / empty entries are skipped; all-bad → nil.
	if r := parseRateRules("  ,x/2m,5/,0/1m,-1/1m,7/nope"); len(r) != 0 {
		t.Fatalf("malformed spec should yield no rules, got %+v", r)
	}
	if newRateLimiter(parseRateRules("")) != nil {
		t.Fatal("empty spec should disable the limiter (nil)")
	}
}

func TestRateLimiterDualWindow(t *testing.T) {
	rl := newRateLimiter(parseRateRules("100/2m,200/4h"))
	base := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	// Burst: first 100 in the 2m window pass, the 101st is throttled.
	for i := 0; i < 100; i++ {
		if !rl.allow("t1", base) {
			t.Fatalf("send %d should be allowed", i)
		}
	}
	if rl.allow("t1", base) {
		t.Fatal("101st within 2m should be throttled by the burst cap")
	}

	// After 2m the burst window clears, but the 4h sustained cap (200) still
	// counts the first 100 → another 100 pass, then the 201st is throttled.
	later := base.Add(2*time.Minute + time.Second)
	for i := 0; i < 100; i++ {
		if !rl.allow("t1", later) {
			t.Fatalf("post-burst send %d should be allowed (sustained has room)", i)
		}
	}
	if rl.allow("t1", later) {
		t.Fatal("201st within 4h should be throttled by the sustained cap")
	}

	// A different tenant is independent.
	if !rl.allow("t2", later) {
		t.Fatal("separate tenant must not be throttled by t1's usage")
	}

	// After the longest (4h) window fully elapses, t1 is clear again.
	if !rl.allow("t1", base.Add(4*time.Hour+time.Second)) {
		t.Fatal("after 4h the sustained window should clear")
	}
}

func TestRateLimiterDisabledNil(t *testing.T) {
	var rl *rateLimiter // nil → callers gate on rl != nil; never invoked
	if rl != nil {
		t.Fatal("expected nil limiter")
	}
}
