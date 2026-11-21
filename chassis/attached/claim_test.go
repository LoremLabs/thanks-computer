package attached

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kvtools/valkeyrie"

	"github.com/loremlabs/thanks-computer/chassis/kv"
	boltdb "github.com/loremlabs/thanks-computer/chassis/kv/boltstore"
)

const (
	tTenant = "tnt_test"
	tW      = "0f3a" // a workspace id; any key segment will do here
)

func testClaims(t *testing.T) *Claims {
	t.Helper()
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewClaims(kv.New(s, 0, 0))
}

func conn(gen int64) Claim {
	return Claim{Gen: gen, Node: fmt.Sprintf("node-%d", gen), Conn: fmt.Sprintf("conn-%d", gen)}
}

func mustInstall(t *testing.T, c *Claims, mine Claim) Claim {
	t.Helper()
	written, _, err := c.Install(context.Background(), tTenant, tW, mine)
	if err != nil {
		t.Fatalf("install gen %d: %v", mine.Gen, err)
	}
	return written
}

func mustRefuse(t *testing.T, c *Claims, mine Claim) {
	t.Helper()
	if _, _, err := c.Install(context.Background(), tTenant, tW, mine); !errors.Is(err, ErrReplaced) {
		t.Fatalf("install gen %d: err = %v, want ErrReplaced", mine.Gen, err)
	}
}

// holder asserts who holds W: gen 0 means nobody.
func holder(t *testing.T, c *Claims, gen int64) {
	t.Helper()
	cur, found, err := c.Read(context.Background(), tTenant, tW)
	if err != nil {
		t.Fatal(err)
	}
	live := found && cur.Live(c.now())
	switch {
	case gen == 0 && live:
		t.Fatalf("gen %d holds the name, want nobody", cur.Gen)
	case gen != 0 && (!live || cur.Gen != gen || cur.Conn != conn(gen).Conn):
		t.Fatalf("holder = %+v (found %v), want gen %d", cur, found, gen)
	}
}

func TestGenerationsAreIssuedOnce(t *testing.T) {
	c := testClaims(t)
	ctx := context.Background()
	seen := map[int64]bool{}
	var last int64
	for i := 0; i < 20; i++ {
		g, err := c.NextGen(ctx, tTenant, tW)
		if err != nil {
			t.Fatal(err)
		}
		if seen[g] || g <= last {
			t.Fatalf("generation %d after %d", g, last)
		}
		seen[g], last = true, g
	}
	// Another workspace counts on its own.
	if g, _ := c.NextGen(ctx, tTenant, "other"); g != 1 {
		t.Fatalf("another workspace's first generation = %d", g)
	}
}

// Every order in which an older connection A (10) and a newer one B (11) can
// read and write ends with B holding the name, and A told it was replaced.
func TestOlderGenerationNeverHolds(t *testing.T) {
	A, B := conn(10), conn(11)

	t.Run("A then B", func(t *testing.T) {
		c := testClaims(t)
		a := mustInstall(t, c, A)
		_, replaced, err := c.Install(context.Background(), tTenant, tW, B)
		if err != nil {
			t.Fatal(err)
		}
		if replaced == nil || replaced.Conn != A.Conn {
			t.Fatalf("replaced = %+v, want A named so its node can be told", replaced)
		}
		holder(t, c, 11)
		if _, err := c.Renew(context.Background(), tTenant, tW, a); !errors.Is(err, ErrReplaced) {
			t.Fatalf("A's renewal after B: %v", err)
		}
		holder(t, c, 11)
	})

	t.Run("B then A", func(t *testing.T) {
		c := testClaims(t)
		mustInstall(t, c, B)
		mustRefuse(t, c, A)
		holder(t, c, 11)
	})

	// The case the comparison exists for. A reads first and writes last.
	// Compare-and-swap alone would refuse A's swap here too, but only because
	// the value changed; the next case shows what it cannot refuse.
	t.Run("A reads, B installs, A swaps", func(t *testing.T) {
		c := testClaims(t)
		once := false
		c.afterRead = func(gen int64) {
			if gen == A.Gen && !once {
				once = true
				mustInstall(t, c, B)
			}
		}
		mustRefuse(t, c, A)
		holder(t, c, 11)
	})

	// A reads LAST: it sees B's claim, unchanged since, and a bare
	// compare-and-swap from that value would succeed. Only comparing the
	// generations keeps A out.
	t.Run("B installs, A reads last", func(t *testing.T) {
		c := testClaims(t)
		mustInstall(t, c, B)
		c.afterRead = func(gen int64) {
			if gen == A.Gen {
				t.Fatal("A got as far as the swap over a newer generation")
			}
		}
		mustRefuse(t, c, A)
		holder(t, c, 11)
	})

	t.Run("both read nothing, A swaps first", func(t *testing.T) {
		c := testClaims(t)
		once := false
		c.afterRead = func(gen int64) {
			if gen == B.Gen && !once {
				once = true
				mustInstall(t, c, A) // lands between B's read and B's swap
			}
		}
		mustInstall(t, c, B) // loses its swap, reads A, compares, swaps over it
		holder(t, c, 11)
	})

	t.Run("both read nothing, B swaps first", func(t *testing.T) {
		c := testClaims(t)
		once := false
		c.afterRead = func(gen int64) {
			if gen == A.Gen && !once {
				once = true
				mustInstall(t, c, B)
			}
		}
		mustRefuse(t, c, A)
		holder(t, c, 11)
	})
}

// A release, an expired lease and a destroy all leave the generation behind,
// so a connection from before them still cannot install.
func TestOlderGenerationNeverHoldsAfterTheNewerIsGone(t *testing.T) {
	A, B, C := conn(10), conn(11), conn(12)
	ctx := context.Background()

	t.Run("after a release", func(t *testing.T) {
		c := testClaims(t)
		b := mustInstall(t, c, B)
		released, err := c.Release(ctx, tTenant, tW, b)
		if err != nil || !released {
			t.Fatalf("release = %v, %v", released, err)
		}
		holder(t, c, 0)
		mustRefuse(t, c, A)
		holder(t, c, 0)
		// B itself does not come back either: a released connection is done.
		mustRefuse(t, c, B)
		// A newer one does.
		mustInstall(t, c, C)
		holder(t, c, 12)
	})

	t.Run("after its lease ran out", func(t *testing.T) {
		c := testClaims(t)
		now := time.Now()
		c.now = func() time.Time { return now }
		b := mustInstall(t, c, B)
		now = now.Add(c.Lease() + time.Second) // B's node died without a release
		holder(t, c, 0)
		mustRefuse(t, c, A)
		holder(t, c, 0)
		// B's own renewal still works: nothing replaced it.
		if _, err := c.Renew(ctx, tTenant, tW, b); err != nil {
			t.Fatalf("B's late renewal: %v", err)
		}
		holder(t, c, 11)
	})

	t.Run("after a destroy", func(t *testing.T) {
		c := testClaims(t)
		b := mustInstall(t, c, B)
		if err := c.Clear(ctx, tTenant, tW); err != nil {
			t.Fatal(err)
		}
		holder(t, c, 0)
		mustRefuse(t, c, A)
		if _, err := c.Renew(ctx, tTenant, tW, b); !errors.Is(err, ErrReplaced) {
			t.Fatalf("B's renewal after a destroy: %v", err)
		}
		if err := c.Clear(ctx, tTenant, tW); err != nil {
			t.Fatalf("a second clear: %v", err)
		}
		mustInstall(t, c, C)
		holder(t, c, 12)
	})
}

// An old connection's release and renewal never touch a newer claim.
func TestStaleConnectionCannotTouchANewerClaim(t *testing.T) {
	c := testClaims(t)
	ctx := context.Background()
	a := mustInstall(t, c, conn(10))
	mustInstall(t, c, conn(11))
	released, err := c.Release(ctx, tTenant, tW, a)
	if err != nil || released {
		t.Fatalf("A's release over B = %v, %v, want nothing released", released, err)
	}
	holder(t, c, 11)
	if _, err := c.Renew(ctx, tTenant, tW, a); !errors.Is(err, ErrReplaced) {
		t.Fatalf("A's renewal over B: %v", err)
	}
	holder(t, c, 11)
}

func TestSameConnectionInstallsAgain(t *testing.T) {
	c := testClaims(t)
	first := mustInstall(t, c, conn(7))
	_, replaced, err := c.Install(context.Background(), tTenant, tW, conn(7))
	if err != nil || replaced != nil {
		t.Fatalf("the same connection again: replaced %+v, err %v", replaced, err)
	}
	holder(t, c, 7)
	// Another connection with the SAME generation cannot exist; if one
	// claims to, it is refused.
	twin := conn(7)
	twin.Conn = "conn-twin"
	mustRefuse(t, c, twin)
	_ = first
}

func TestRenewKeepsTheClaim(t *testing.T) {
	c := testClaims(t)
	now := time.Now()
	c.now = func() time.Time { return now }
	mine := mustInstall(t, c, conn(3))
	for i := 0; i < 5; i++ {
		now = now.Add(c.Lease() / 2)
		var err error
		if mine, err = c.Renew(context.Background(), tTenant, tW, mine); err != nil {
			t.Fatalf("renewal %d: %v", i, err)
		}
		holder(t, c, 3)
	}
	// A renewal from a value that is no longer the stored one is refused,
	// even by the holder itself: it must renew from what it last wrote.
	stale := mine
	stale.LeaseUntil = stale.LeaseUntil.Add(-time.Hour)
	if _, err := c.Renew(context.Background(), tTenant, tW, stale); !errors.Is(err, ErrReplaced) {
		t.Fatalf("a renewal from a stale value: %v", err)
	}
}

// Many connections at once, each with its own generation, in no fixed order.
// The generation a reader sees never goes down, and the last one issued ends
// up the holder.
func TestConcurrentInstallsAreMonotonic(t *testing.T) {
	c := testClaims(t)
	ctx := context.Background()
	const n = 24

	var stop atomic.Bool
	var watch sync.WaitGroup
	watch.Add(1)
	var wentDown atomic.Int64
	go func() {
		defer watch.Done()
		var high int64
		for !stop.Load() {
			cur, found, err := c.Read(ctx, tTenant, tW)
			if err == nil && found {
				if cur.Gen < high {
					wentDown.Store(high<<32 | cur.Gen)
					return
				}
				high = cur.Gen
			}
		}
	}()

	var wg sync.WaitGroup
	var top atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := c.NextGen(ctx, tTenant, tW)
			if err != nil {
				t.Error(err)
				return
			}
			for {
				if cur := top.Load(); g <= cur || top.CompareAndSwap(cur, g) {
					break
				}
			}
			_, _, err = c.Install(ctx, tTenant, tW, conn(g))
			if err != nil && !errors.Is(err, ErrReplaced) {
				t.Errorf("install gen %d: %v", g, err)
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	watch.Wait()

	if v := wentDown.Load(); v != 0 {
		t.Fatalf("the installed generation went down, from %d to %d", v>>32, v&0xffffffff)
	}
	if top.Load() != n {
		t.Fatalf("highest generation issued = %d, want %d", top.Load(), n)
	}
	holder(t, c, n)
}
