// Package attached is a workspace held by a machine the fleet did not make:
// one that dialed in with `txco attach` and proved a key enrolled for that
// one workspace name.
//
// (The name `attach` was taken twice over: the workspace verb that binds a
// terminal, and the package chassis/attach behind it.)
package attached

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/kv"
)

// The claim says which connection holds a workspace NOW. It is liveness
// only. Whether a name is attached at all is the workspace's row in the
// runtime DB, never this: a missing or released claim reads "away", and
// nothing else.
//
// THE INVARIANT: a connection with an older generation never becomes the
// holder, whatever the order of events.
//
// Three things keep it:
//
//   - A generation is issued once. It comes from a counter that never
//     expires and is never reset.
//   - Installing COMPARES generations. Compare-and-swap alone only proves
//     that nothing changed since the read; it would let generation 10 write
//     over 11 if 10 happened to read last. So an install refuses when the
//     installed generation is its own or higher, and only then swaps.
//   - The key never disappears. A release writes a marker that keeps the
//     generation and has no expiry, so the highest generation ever installed
//     is always there to compare with.
const (
	// claimNS and genNS are chassis-reserved KV namespaces (kv.IsReservedNamespace):
	// no rule can read or write them through txco://kv.
	claimNS = "_txc.attach"
	genNS   = "_txc.attach.gen"

	// DefaultLease is how long a claim stays live without a renewal. The
	// holder renews past half of it.
	DefaultLease = 10 * time.Minute

	// installAttempts bounds an install's read-compare-swap loop. Each lost
	// swap means another connection wrote in between; one with a higher
	// generation ends the loop at the next compare.
	installAttempts = 32
)

// ErrReplaced: a newer connection holds the name, or held it last. The
// connection that gets this closes as `replaced` and stops serving.
var ErrReplaced = errors.New("attached: replaced by a newer connection")

// Claim is the value at <tenant>/_txc.attach/<W>.
type Claim struct {
	// Gen is the generation of the connection that wrote this.
	Gen int64 `json:"gen"`
	// Released: no connection holds the name. Gen is still the highest
	// generation ever installed.
	Released bool `json:"released,omitempty"`

	Node       string    `json:"node,omitempty"`
	Conn       string    `json:"conn,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitzero"`

	// What the machine said of itself, for status. Never trusted for a
	// decision.
	Machine string    `json:"machine,omitempty"`
	Root    string    `json:"root,omitempty"`
	OS      string    `json:"os,omitempty"`
	Version string    `json:"version,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// Live reports whether the claim names a connection whose lease has not run
// out at now.
func (c Claim) Live(now time.Time) bool {
	return !c.Released && c.Conn != "" && now.Before(c.LeaseUntil)
}

// Holds reports whether the claim is this connection's own, live or not.
func (c Claim) Holds(conn string, gen int64) bool {
	return !c.Released && c.Gen == gen && c.Conn == conn
}

// Claims reads and writes claims in the KV.
type Claims struct {
	kv    *kv.KV
	lease time.Duration
	now   func() time.Time

	// afterRead, in tests, runs between an install's read and its swap: the
	// window every interleaving lives in.
	afterRead func(gen int64)
}

// NewClaims returns the claims over k, with DefaultLease.
func NewClaims(k *kv.KV) *Claims {
	return &Claims{kv: k, lease: DefaultLease, now: time.Now}
}

// Lease is how long a written claim stays live.
func (c *Claims) Lease() time.Duration { return c.lease }

// NextGen issues the next generation for W. Call it only after the
// connection has proved its key: the generation orders connections that are
// entitled to the name.
func (c *Claims) NextGen(ctx context.Context, tenant, w string) (int64, error) {
	// No TTL: the counter must never start again.
	for attempt := 0; ; attempt++ {
		g, err := c.kv.Incr(ctx, tenant, genNS, w, 1, 0)
		if err == nil || !errors.Is(err, kv.ErrContention) || attempt >= installAttempts {
			return g, err
		}
		if err := backoff(ctx, attempt); err != nil {
			return 0, err
		}
	}
}

// backoff waits a little longer each attempt, so writers that collided do
// not collide again in step.
func backoff(ctx context.Context, attempt int) error {
	d := time.Duration(attempt+1) * time.Millisecond
	if d > 20*time.Millisecond {
		d = 20 * time.Millisecond
	}
	d += time.Duration(rand.Int64N(int64(d)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read returns W's claim. found is false when no connection ever installed
// one. A caller that needs the holder checks Live as well.
func (c *Claims) Read(ctx context.Context, tenant, w string) (claim Claim, found bool, err error) {
	raw, found, err := c.kv.Get(ctx, tenant, claimNS, w)
	if err != nil || !found {
		return Claim{}, false, err
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return Claim{}, false, fmt.Errorf("attached: corrupt claim for %s: %w", w, err)
	}
	return claim, true, nil
}

// Install makes mine the holder of W, with a fresh lease. It returns the
// claim it wrote, and the claim it replaced when that one named a live
// connection (so the caller can tell that connection's node).
//
// It refuses with ErrReplaced when the installed generation is mine's own or
// higher — including a released marker of that generation, so a connection
// that has released never comes back. The one exception is the same
// connection installing again over its own live claim, which only renews it.
func (c *Claims) Install(ctx context.Context, tenant, w string, mine Claim) (written Claim, replaced *Claim, err error) {
	if mine.Gen <= 0 || mine.Conn == "" || mine.Node == "" {
		return Claim{}, nil, errors.New("attached: a claim needs a generation, a connection and a node")
	}
	mine.Released = false
	for attempt := 0; attempt < installAttempts; attempt++ {
		cur, found, err := c.Read(ctx, tenant, w)
		if err != nil {
			return Claim{}, nil, err
		}
		if found {
			switch {
			case cur.Gen > mine.Gen:
				return Claim{}, nil, ErrReplaced
			case cur.Gen == mine.Gen && !cur.Holds(mine.Conn, mine.Gen):
				return Claim{}, nil, ErrReplaced
			}
		}
		if c.afterRead != nil {
			c.afterRead(mine.Gen)
		}
		next := mine
		next.LeaseUntil = c.now().Add(c.lease).UTC()
		if next.Since.IsZero() {
			next.Since = c.now().UTC()
		}
		swapped, err := c.swap(ctx, tenant, w, found, cur, next)
		if err != nil {
			return Claim{}, nil, err
		}
		if !swapped {
			continue // someone wrote in between: read and compare again
		}
		if found && cur.Live(c.now()) && !cur.Holds(mine.Conn, mine.Gen) {
			prev := cur
			return next, &prev, nil
		}
		return next, nil, nil
	}
	return Claim{}, nil, fmt.Errorf("attached: claim for %s is contended", w)
}

// Renew extends mine's lease. mine must be the exact claim this connection
// last wrote: the swap is from that value, so any other write since — a
// newer connection, a release, a destroy — fails it with ErrReplaced.
func (c *Claims) Renew(ctx context.Context, tenant, w string, mine Claim) (Claim, error) {
	next := mine
	next.LeaseUntil = c.now().Add(c.lease).UTC()
	swapped, err := c.swap(ctx, tenant, w, true, mine, next)
	if err != nil {
		return Claim{}, err
	}
	if !swapped {
		return Claim{}, ErrReplaced
	}
	return next, nil
}

// Release marks W as held by no one, keeping mine's generation. It writes
// only over mine's own exact value, so an old connection's release never
// touches a newer claim. released is false when the claim was no longer
// mine: nothing was changed, and nothing needed to be.
func (c *Claims) Release(ctx context.Context, tenant, w string, mine Claim) (released bool, err error) {
	return c.swap(ctx, tenant, w, true, mine, Claim{Gen: mine.Gen, Released: true})
}

// Clear marks W as held by no one, whoever holds it: the workspace was
// destroyed or its credential revoked. The marker keeps the highest
// generation installed, so a connection from before can never install after.
func (c *Claims) Clear(ctx context.Context, tenant, w string) error {
	for attempt := 0; attempt < installAttempts; attempt++ {
		cur, found, err := c.Read(ctx, tenant, w)
		if err != nil {
			return err
		}
		if !found || cur.Released {
			return nil
		}
		swapped, err := c.swap(ctx, tenant, w, true, cur, Claim{Gen: cur.Gen, Released: true})
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return fmt.Errorf("attached: claim for %s is contended", w)
}

// swap writes next over prev (or over nothing, when !found) and nothing
// else. No TTL, ever: the key must not disappear.
func (c *Claims) swap(ctx context.Context, tenant, w string, found bool, prev, next Claim) (bool, error) {
	nextRaw, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	var prevRaw json.RawMessage
	if found {
		if prevRaw, err = json.Marshal(prev); err != nil {
			return false, err
		}
	}
	swapped, _, err := c.kv.CAS(ctx, tenant, claimNS, w, !found, prevRaw, nextRaw, 0)
	if errors.Is(err, kv.ErrContention) {
		// Lost to other writers every time: the same as a lost swap. The
		// caller reads and compares again.
		return false, backoff(ctx, 0)
	}
	return swapped, err
}
