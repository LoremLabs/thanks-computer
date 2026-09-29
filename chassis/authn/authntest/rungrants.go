package authntest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

// clock is a settable time source for a store under test.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// holder makes a principal managed by stack that holds a standing grant on
// each of the named resources.
func holder(t testing.TB, s *authn.Store, tenantID, stack, name string, resources ...string) authn.Principal {
	t.Helper()
	p := service(t, s, tenantID, stack, name)
	hold(t, s, tenantID, stack, p, resources...)
	return p
}

func hold(t testing.TB, s *authn.Store, tenantID, stack string, p authn.Principal, resources ...string) {
	t.Helper()
	for _, raw := range resources {
		res, err := authn.ParseResource(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.PutGrant(context.Background(), tenantID, stack, p, authn.NewGrant{Kind: res.Kind, Name: res.Name}); err != nil {
			t.Fatalf("grant %s to %s: %v", raw, p.ID, err)
		}
	}
}

// work is a mint request with the fields a test does not care about filled.
func work(run string, allow ...string) authn.NewRunGrant {
	return authn.NewRunGrant{Run: run, Stack: "web", Allow: allow, BudgetCalls: 10, TTL: 10 * time.Minute}
}

func runGrantCases(t *testing.T, newStore func(t *testing.T) *authn.Store) {
	ctx := context.Background()

	t.Run("mint writes the row the work is held to", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		at := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
		s.SetClock(at.now)
		p := holder(t, s, tn, "web", "research", "secret:CRM_KEY", "crm.lookup", "secret:DB_DSN")

		in := work("task-1", "secret:CRM_KEY", " crm.lookup ", "capability:crm.lookup", "secret:CRM_KEY")
		in.Stack, in.Workspace, in.WorkspaceID, in.TraceID = "web/canary", "bench", "abc123", "tr_1"
		g, err := s.MintRunGrant(ctx, tn, "web/canary/_mail", p, in)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(g.ID, "rgr_") || g.TenantID != tn || g.Principal != p || g.MintedBy != "web" ||
			g.Stack != "web/canary" || g.Run != "task-1" || g.Generation != 1 || g.Workspace != "bench" ||
			g.WorkspaceID != "abc123" || g.NodeClass != authn.NodeUnreviewed || g.BudgetCalls != 10 ||
			g.SpentCalls != 0 || g.ParentGrant != "" || g.Depth != 0 || g.TraceID != "tr_1" {
			t.Errorf("grant = %+v", g)
		}
		// The allowlist is de-duplicated and sorted, in its canonical form.
		if got := strings.Join(g.AllowStrings(), " "); got != "crm.lookup secret:CRM_KEY" {
			t.Errorf("allow = %q", got)
		}
		if !g.Allows(secret("CRM_KEY")) || !g.Allows(capability("crm.lookup")) || g.Allows(secret("DB_DSN")) {
			t.Errorf("Allows disagrees with the allowlist: %+v", g.Allow)
		}
		if !g.IssuedAt.Equal(at.now()) || !g.ExpiresAt.Equal(at.now().Add(10*time.Minute)) || !g.Live(at.now()) {
			t.Errorf("issued %s, expires %s", g.IssuedAt, g.ExpiresAt)
		}
		if len(g.FileKey) != 32 || strings.Contains(g.ID, g.FileKey) {
			t.Errorf("file key = %q", g.FileKey)
		}

		// What was stored is what was returned.
		got, err := s.ReadRunGrant(ctx, tn, g.ID)
		if err != nil || got.ID != g.ID || got.FileKey != g.FileKey || strings.Join(got.AllowStrings(), " ") != "crm.lookup secret:CRM_KEY" ||
			!got.ExpiresAt.Equal(g.ExpiresAt) || got.Stack != "web/canary" {
			t.Errorf("read: %+v err=%v", got, err)
		}
		// Two grants never share a file key.
		other, err := s.MintRunGrant(ctx, tn, "web", p, work("task-2", "crm.lookup"))
		if err != nil || other.FileKey == g.FileKey || other.Generation != 1 {
			t.Errorf("second run: %+v err=%v", other, err)
		}
		// A reviewed node is the minter's promise.
		rin := work("task-3", "crm.lookup")
		rin.NodeClass = authn.NodeReviewed
		if rg, err := s.MintRunGrant(ctx, tn, "web", p, rin); err != nil || rg.NodeClass != authn.NodeReviewed {
			t.Errorf("reviewed: %+v err=%v", rg, err)
		}
	})

	t.Run("a mint cannot exceed the principal's standing grants", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "secret:CRM_KEY")

		for name, allow := range map[string][]string{
			"a secret it does not hold":     {"secret:DB_DSN"},
			"one held, one not":             {"secret:CRM_KEY", "secret:DB_DSN"},
			"a capability it does not hold": {"crm.lookup"},
			"the capability of that name":   {"capability:CRM_KEY"},
		} {
			_, err := s.MintRunGrant(ctx, tn, "web", p, work("task-"+strings.NewReplacer(" ", "-", ",", "").Replace(name), allow...))
			if name == "the capability of that name" {
				// Not even a well-formed capability name.
				if !errors.Is(err, authn.ErrInvalid) {
					t.Errorf("%s: %v", name, err)
				}
				continue
			}
			if !errors.Is(err, authn.ErrExceedsStanding) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// The refusal names what is missing, for the author.
		_, err := s.MintRunGrant(ctx, tn, "web", p, work("task-x", "secret:CRM_KEY", "secret:DB_DSN"))
		if err == nil || !strings.Contains(err.Error(), "secret:DB_DSN") || strings.Contains(err.Error(), "CRM_KEY") {
			t.Errorf("refusal should name only the missing grant: %v", err)
		}

		// A revoked standing grant no longer counts…
		if _, _, err := s.RevokeGrantOn(ctx, tn, "web", p, authn.ResourceSecret, "CRM_KEY"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MintRunGrant(ctx, tn, "web", p, work("task-y", "secret:CRM_KEY")); !errors.Is(err, authn.ErrExceedsStanding) {
			t.Errorf("mint on a revoked standing grant: %v", err)
		}
		// …nor does a disabled user's.
		u, _, _, _ := s.CreateUser(ctx, tn, "web", authn.NewUser{Email: "alice@example.com"})
		hold(t, s, tn, "web", u.Principal(), "secret:CRM_KEY")
		if _, err := s.MintRunGrant(ctx, tn, "web", u.Principal(), work("task-u", "secret:CRM_KEY")); err != nil {
			t.Fatalf("mint for a user: %v", err)
		}
		_, _ = s.SetUserDisabled(ctx, tn, "web", u.ID, true)
		if _, err := s.MintRunGrant(ctx, tn, "web", u.Principal(), work("task-u", "secret:CRM_KEY")); !errors.Is(err, authn.ErrDisabled) {
			t.Errorf("mint for a disabled user: %v", err)
		}
	})

	t.Run("only the stack that manages a principal mints for it", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		var oe *authn.OwnerError
		if _, err := s.MintRunGrant(ctx, tn, "ingest", p, work("task-1", "crm.lookup")); !errors.As(err, &oe) || oe.Owner != "web" {
			t.Errorf("mint from another stack: %v", err)
		}
		if _, err := s.MintRunGrant(ctx, tn, "", p, work("task-1", "crm.lookup")); !errors.Is(err, authn.ErrNoStack) {
			t.Errorf("mint with no stack: %v", err)
		}
		ghost, _ := authn.ParsePrincipal("service:ghost")
		if _, err := s.MintRunGrant(ctx, tn, "web", ghost, work("task-1", "crm.lookup")); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("mint for an unwritten principal: %v", err)
		}

		g, err := s.MintRunGrant(ctx, tn, "web", p, work("task-1", "crm.lookup"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetRunGrant(ctx, tn, "ingest", g.ID); !errors.As(err, &oe) || oe.Owner != "web" {
			t.Errorf("get from another stack: %v", err)
		}
		if _, _, err := s.RevokeRunGrant(ctx, tn, "ingest", g.ID); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("revoke from another stack: %v", err)
		}
		if _, _, err := s.CloseRunGrant(ctx, tn, "ingest", g.ID, ""); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("close from another stack: %v", err)
		}
		if _, err := s.GetRunGrant(ctx, tn, "", g.ID); !errors.Is(err, authn.ErrNoStack) {
			t.Errorf("get with no stack: %v", err)
		}
		if got, err := s.GetRunGrant(ctx, tn, "web/canary", g.ID); err != nil || got.ID != g.ID {
			t.Errorf("get from the stack's slot: %+v err=%v", got, err)
		}
		// Another tenant knows nothing of it.
		other := tenant()
		if _, err := s.ReadRunGrant(ctx, other, g.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant read: %v", err)
		}
		if _, _, err := s.RevokeRunGrant(ctx, other, "web", g.ID); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant revoke: %v", err)
		}
		if err := s.ChargeRunGrant(ctx, other, g.ID, 1, true); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("cross-tenant charge: %v", err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, g.ID); !got.Live(time.Now()) || got.SpentCalls != 0 {
			t.Errorf("a refused call touched the grant: %+v", got)
		}
	})

	t.Run("minting a run again closes its older grant", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		first, err := s.MintRunGrant(ctx, tn, "web", p, work("task-1", "crm.lookup"))
		if err != nil {
			t.Fatal(err)
		}
		// Another run on the same workspace is left alone.
		beside, _ := s.MintRunGrant(ctx, tn, "web", p, work("task-2", "crm.lookup"))

		second, err := s.MintRunGrant(ctx, tn, "web/canary", p, work("task-1", "crm.lookup"))
		if err != nil || second.Generation != 2 || second.ID == first.ID {
			t.Fatalf("second mint: %+v err=%v", second, err)
		}
		old, _ := s.ReadRunGrant(ctx, tn, first.ID)
		if old.Live(time.Now()) || old.ClosedAt == nil || old.CloseReason != "superseded" || old.RevokedAt != nil {
			t.Errorf("the older grant: %+v", old)
		}
		if err := s.ChargeRunGrant(ctx, tn, first.ID, 1, true); !errors.Is(err, authn.ErrRunNotLive) {
			t.Errorf("charge on a superseded grant: %v", err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, beside.ID); !got.Live(time.Now()) {
			t.Errorf("another run's grant was closed: %+v", got)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, second.ID); !got.Live(time.Now()) {
			t.Errorf("the new grant is not live: %+v", got)
		}

		// A caller with a counter of its own passes it, and it only goes up.
		in := work("task-1", "crm.lookup")
		in.Generation = 7
		third, err := s.MintRunGrant(ctx, tn, "web", p, in)
		if err != nil || third.Generation != 7 {
			t.Fatalf("explicit generation: %+v err=%v", third, err)
		}
		for _, stale := range []int64{7, 6, 1} {
			in.Generation = stale
			if _, err := s.MintRunGrant(ctx, tn, "web", p, in); !errors.Is(err, authn.ErrStaleGeneration) {
				t.Errorf("generation %d after 7: %v", stale, err)
			}
		}
		// A refused mint closed nothing.
		if got, _ := s.ReadRunGrant(ctx, tn, third.ID); !got.Live(time.Now()) {
			t.Errorf("a stale mint closed the live grant: %+v", got)
		}
		in.Generation = 0
		if next, err := s.MintRunGrant(ctx, tn, "web", p, in); err != nil || next.Generation != 8 {
			t.Errorf("the next generation: %+v err=%v", next, err)
		}
		// Runs are per stack: another stack's `task-1` is another run.
		q := holder(t, s, tn, "ingest", "loader", "crm.lookup")
		in = work("task-1", "crm.lookup")
		in.Stack = "ingest"
		if g, err := s.MintRunGrant(ctx, tn, "ingest", q, in); err != nil || g.Generation != 1 {
			t.Errorf("another stack's run of the same name: %+v err=%v", g, err)
		}
	})

	t.Run("a grant expires, and can be closed or revoked", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		at := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
		s.SetClock(at.now)
		p := holder(t, s, tn, "web", "research", "crm.lookup")

		g, _ := s.MintRunGrant(ctx, tn, "web", p, work("expires", "crm.lookup"))
		at.advance(10*time.Minute - time.Second)
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); err != nil {
			t.Errorf("charge in the last second: %v", err)
		}
		at.advance(time.Second)
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); !errors.Is(err, authn.ErrRunNotLive) {
			t.Errorf("charge at expiry: %v", err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, g.ID); got.Live(at.now()) || got.SpentCalls != 1 {
			t.Errorf("expired grant: %+v", got)
		}

		g, _ = s.MintRunGrant(ctx, tn, "web", p, work("closes", "crm.lookup"))
		got, closed, err := s.CloseRunGrant(ctx, tn, "web", g.ID, " failed: exit 2 ")
		if err != nil || !closed || got.ClosedAt == nil || got.CloseReason != "failed: exit 2" || got.Live(at.now()) {
			t.Fatalf("close: %+v closed=%v err=%v", got, closed, err)
		}
		if _, again, err := s.CloseRunGrant(ctx, tn, "web", g.ID, "done"); err != nil || again {
			t.Errorf("second close: closed=%v err=%v", again, err)
		}
		if stored, _ := s.ReadRunGrant(ctx, tn, g.ID); stored.CloseReason != "failed: exit 2" {
			t.Errorf("a second close rewrote the reason: %+v", stored)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); !errors.Is(err, authn.ErrRunNotLive) {
			t.Errorf("charge on a closed grant: %v", err)
		}
		if _, _, err := s.CloseRunGrant(ctx, tn, "web", g.ID, "a\x00b"); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("a control character in the reason: %v", err)
		}
		// A closed grant can still be revoked, for the record.
		if got, revoked, err := s.RevokeRunGrant(ctx, tn, "web", g.ID); err != nil || !revoked || got.RevokedAt == nil {
			t.Errorf("revoke a closed grant: %+v revoked=%v err=%v", got, revoked, err)
		}

		g, _ = s.MintRunGrant(ctx, tn, "web", p, work("revoked", "crm.lookup"))
		if got, closed, err := s.CloseRunGrant(ctx, tn, "web", g.ID, ""); err != nil || !closed || got.CloseReason != "done" {
			t.Errorf("close with no reason: %+v closed=%v err=%v", got, closed, err)
		}
		g, _ = s.MintRunGrant(ctx, tn, "web", p, work("revoked", "crm.lookup"))
		got, revoked, err := s.RevokeRunGrant(ctx, tn, "web/canary", g.ID)
		if err != nil || !revoked || got.RevokedAt == nil || got.Live(at.now()) {
			t.Fatalf("revoke: %+v revoked=%v err=%v", got, revoked, err)
		}
		if _, again, err := s.RevokeRunGrant(ctx, tn, "web", g.ID); err != nil || again {
			t.Errorf("second revoke: revoked=%v err=%v", again, err)
		}
		if _, closed, err := s.CloseRunGrant(ctx, tn, "web", g.ID, ""); err != nil || closed {
			t.Errorf("close a revoked grant: closed=%v err=%v", closed, err)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); !errors.Is(err, authn.ErrRunNotLive) {
			t.Errorf("charge on a revoked grant: %v", err)
		}
		if _, _, err := s.RevokeRunGrant(ctx, tn, "web", "rgr_missing"); !errors.Is(err, authn.ErrNotFound) {
			t.Errorf("revoke a missing grant: %v", err)
		}
	})

	t.Run("a charge stops at the budget", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		in := work("task-1", "crm.lookup")
		in.BudgetCalls = 3
		g, err := s.MintRunGrant(ctx, tn, "web", p, in)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 2, true); err != nil {
			t.Fatal(err)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 2, true); !errors.Is(err, authn.ErrRunBudget) {
			t.Errorf("a charge past the budget: %v", err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, g.ID); got.SpentCalls != 2 || got.Remaining() != 1 {
			t.Errorf("a refused charge spent something: %+v", got)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); err != nil {
			t.Errorf("the last call: %v", err)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true); !errors.Is(err, authn.ErrRunBudget) {
			t.Errorf("one past the last call: %v", err)
		}
		// A caller that was told to go ahead regardless still records it.
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, false); err != nil {
			t.Errorf("an unenforced charge: %v", err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, g.ID); got.SpentCalls != 4 || got.Remaining() != 0 {
			t.Errorf("after an unenforced charge: %+v", got)
		}
		// …but never on a grant that is not live.
		_, _, _ = s.RevokeRunGrant(ctx, tn, "web", g.ID)
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 1, false); !errors.Is(err, authn.ErrRunNotLive) {
			t.Errorf("an unenforced charge on a revoked grant: %v", err)
		}
		if err := s.ChargeRunGrant(ctx, tn, g.ID, 0, true); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("a charge of nothing: %v", err)
		}
	})

	t.Run("racing for the last call charges once", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		in := work("task-1", "crm.lookup")
		in.BudgetCalls = 5
		g, err := s.MintRunGrant(ctx, tn, "web", p, in)
		if err != nil {
			t.Fatal(err)
		}
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			charged int
		)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := s.ChargeRunGrant(ctx, tn, g.ID, 1, true)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					charged++
				case !errors.Is(err, authn.ErrRunBudget):
					t.Errorf("charge: %v", err)
				}
			}()
		}
		wg.Wait()
		if got, _ := s.ReadRunGrant(ctx, tn, g.ID); charged != 5 || got.SpentCalls != 5 {
			t.Errorf("charged %d times, spent %d, of a budget of 5", charged, got.SpentCalls)
		}
	})

	t.Run("a grant narrowed from another only narrows", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		at := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
		s.SetClock(at.now)
		p := holder(t, s, tn, "web", "research", "crm.lookup", "mail.send", "secret:CRM_KEY")
		helper := holder(t, s, tn, "web", "helper", "crm.lookup", "mail.send", "secret:CRM_KEY", "secret:DB_DSN")

		in := work("task-1", "crm.lookup", "secret:CRM_KEY")
		in.BudgetCalls = 10
		parent, err := s.MintRunGrant(ctx, tn, "web", p, in)
		if err != nil {
			t.Fatal(err)
		}
		at.advance(4 * time.Minute)

		// The work hands part of itself to another principal.
		cin := work("task-1/lookup", "crm.lookup")
		cin.BudgetCalls, cin.Parent, cin.TTL = 4, parent.ID, time.Hour
		child, err := s.MintRunGrant(ctx, tn, "web", helper, cin)
		if err != nil {
			t.Fatal(err)
		}
		if child.ParentGrant != parent.ID || child.Depth != 1 || child.Principal != helper || child.BudgetCalls != 4 {
			t.Errorf("child = %+v", child)
		}
		// It expires no later than its parent, whatever it asked for.
		if !child.ExpiresAt.Equal(parent.ExpiresAt) {
			t.Errorf("child expires %s, parent %s", child.ExpiresAt, parent.ExpiresAt)
		}
		// Its budget came out of the parent's.
		if got, _ := s.ReadRunGrant(ctx, tn, parent.ID); got.SpentCalls != 4 || got.Remaining() != 6 {
			t.Errorf("parent after the child's mint: %+v", got)
		}

		for name, tc := range map[string]struct {
			mut  func(*authn.NewRunGrant)
			who  authn.Principal
			want error
		}{
			"a name the parent does not hold":  {func(n *authn.NewRunGrant) { n.Allow = []string{"mail.send"} }, helper, authn.ErrExceedsParent},
			"a name only the child could hold": {func(n *authn.NewRunGrant) { n.Allow = []string{"secret:DB_DSN"} }, helper, authn.ErrExceedsParent},
			"more budget than is left":         {func(n *authn.NewRunGrant) { n.BudgetCalls = 7 }, helper, authn.ErrExceedsParent},
			"the parent's own run":             {func(n *authn.NewRunGrant) { n.Run = "task-1" }, helper, authn.ErrInvalid},
			"a parent that does not exist":     {func(n *authn.NewRunGrant) { n.Parent = "rgr_missing" }, helper, authn.ErrParentNotLive},
		} {
			n := work("task-1/"+strings.ReplaceAll(name, " ", "-"), "crm.lookup")
			n.BudgetCalls, n.Parent = 1, parent.ID
			tc.mut(&n)
			if _, err := s.MintRunGrant(ctx, tn, "web", tc.who, n); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v, want %v", name, err, tc.want)
			}
		}
		// The parent's allowlist is not enough: the child's principal must
		// hold the standing grant too.
		bare := service(t, s, tn, "web", "bare")
		n := work("task-1/bare", "crm.lookup")
		n.BudgetCalls, n.Parent = 1, parent.ID
		if _, err := s.MintRunGrant(ctx, tn, "web", bare, n); !errors.Is(err, authn.ErrExceedsStanding) {
			t.Errorf("a child whose principal holds nothing: %v", err)
		}
		// Another stack cannot narrow from a grant it did not mint.
		far := holder(t, s, tn, "ingest", "loader", "crm.lookup")
		n = work("task-1/far", "crm.lookup")
		n.BudgetCalls, n.Parent, n.Stack = 1, parent.ID, "ingest"
		if _, err := s.MintRunGrant(ctx, tn, "ingest", far, n); !errors.Is(err, authn.ErrNotOwner) {
			t.Errorf("narrowing another stack's grant: %v", err)
		}
		// None of the refusals took anything from the parent.
		if got, _ := s.ReadRunGrant(ctx, tn, parent.ID); got.SpentCalls != 4 {
			t.Errorf("a refused mint spent the parent's budget: %+v", got)
		}

		// Depth stops at MaxRunDepth.
		at2 := child
		for depth := 2; depth <= authn.MaxRunDepth; depth++ {
			n := work("task-1/deep-"+string(rune('0'+depth)), "crm.lookup")
			n.BudgetCalls, n.Parent = 1, at2.ID
			next, err := s.MintRunGrant(ctx, tn, "web", helper, n)
			if err != nil || next.Depth != depth {
				t.Fatalf("depth %d: %+v err=%v", depth, next, err)
			}
			at2 = next
		}
		n = work("task-1/too-deep", "crm.lookup")
		n.BudgetCalls, n.Parent = 1, at2.ID
		if _, err := s.MintRunGrant(ctx, tn, "web", helper, n); !errors.Is(err, authn.ErrDepth) {
			t.Errorf("one past the depth: %v", err)
		}

		// Ending the parent ends everything narrowed from it, all the way down.
		if _, revoked, err := s.RevokeRunGrant(ctx, tn, "web", parent.ID); err != nil || !revoked {
			t.Fatalf("revoke the parent: revoked=%v err=%v", revoked, err)
		}
		for _, id := range []string{child.ID, at2.ID} {
			if got, _ := s.ReadRunGrant(ctx, tn, id); got.Live(at.now()) || got.RevokedAt == nil {
				t.Errorf("descendant %s outlived its parent: %+v", id, got)
			}
			if err := s.ChargeRunGrant(ctx, tn, id, 1, true); !errors.Is(err, authn.ErrRunNotLive) {
				t.Errorf("charge on a descendant of a revoked grant: %v", err)
			}
		}
		n = work("task-1/late", "crm.lookup")
		n.BudgetCalls, n.Parent = 1, parent.ID
		if _, err := s.MintRunGrant(ctx, tn, "web", helper, n); !errors.Is(err, authn.ErrParentNotLive) {
			t.Errorf("narrowing a revoked grant: %v", err)
		}
	})

	t.Run("closing or replacing a grant ends what was narrowed from it", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		mint := func(run, parent string) authn.RunGrant {
			t.Helper()
			n := work(run, "crm.lookup")
			n.BudgetCalls, n.Parent = 2, parent
			if parent == "" {
				n.BudgetCalls = 10
			}
			g, err := s.MintRunGrant(ctx, tn, "web", p, n)
			if err != nil {
				t.Fatalf("mint %s: %v", run, err)
			}
			return g
		}
		parent := mint("closing", "")
		child := mint("closing/a", parent.ID)
		if _, closed, err := s.CloseRunGrant(ctx, tn, "web", parent.ID, "done"); err != nil || !closed {
			t.Fatal(err)
		}
		if got, _ := s.ReadRunGrant(ctx, tn, child.ID); got.ClosedAt == nil || got.CloseReason != "done" {
			t.Errorf("child of a closed grant: %+v", got)
		}

		parent = mint("replaced", "")
		child = mint("replaced/a", parent.ID)
		grandchild := mint("replaced/a/b", child.ID)
		beside := mint("unrelated", "")
		mint("replaced", "") // the run is dispatched again
		for _, id := range []string{parent.ID, child.ID, grandchild.ID} {
			if got, _ := s.ReadRunGrant(ctx, tn, id); got.ClosedAt == nil || got.CloseReason != "superseded" {
				t.Errorf("%s after its run was minted again: %+v", id, got)
			}
		}
		if got, _ := s.ReadRunGrant(ctx, tn, beside.ID); !got.Live(time.Now()) {
			t.Errorf("an unrelated grant was closed: %+v", got)
		}
	})

	t.Run("run grant arguments", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		for name, mut := range map[string]func(*authn.NewRunGrant){
			"no run":                 func(n *authn.NewRunGrant) { n.Run = "" },
			"a space in the run":     func(n *authn.NewRunGrant) { n.Run = "task 1" },
			"a slash first":          func(n *authn.NewRunGrant) { n.Run = "/task" },
			"a comma in the run":     func(n *authn.NewRunGrant) { n.Run = "task,1" },
			"run too long":           func(n *authn.NewRunGrant) { n.Run = strings.Repeat("r", 129) },
			"negative generation":    func(n *authn.NewRunGrant) { n.Generation = -1 },
			"no allowlist":           func(n *authn.NewRunGrant) { n.Allow = nil },
			"a wildcard":             func(n *authn.NewRunGrant) { n.Allow = []string{"*"} },
			"a family":               func(n *authn.NewRunGrant) { n.Allow = []string{"crm.*"} },
			"an unknown kind":        func(n *authn.NewRunGrant) { n.Allow = []string{"drive:dc_1"} },
			"too many names":         func(n *authn.NewRunGrant) { n.Allow = manyNames(authn.MaxRunAllow + 1) },
			"no budget":              func(n *authn.NewRunGrant) { n.BudgetCalls = 0 },
			"a negative budget":      func(n *authn.NewRunGrant) { n.BudgetCalls = -1 },
			"a budget past the cap":  func(n *authn.NewRunGrant) { n.BudgetCalls = authn.MaxRunBudget + 1 },
			"no ttl":                 func(n *authn.NewRunGrant) { n.TTL = 0 },
			"a ttl under a second":   func(n *authn.NewRunGrant) { n.TTL = time.Millisecond },
			"a ttl past the cap":     func(n *authn.NewRunGrant) { n.TTL = authn.MaxRunTTL + time.Second },
			"an unknown node class":  func(n *authn.NewRunGrant) { n.NodeClass = "trusted" },
			"a workspace with no id": func(n *authn.NewRunGrant) { n.Workspace = "bench" },
			"an id with no name":     func(n *authn.NewRunGrant) { n.WorkspaceID = "abc" },
		} {
			n := work("task-1", "crm.lookup")
			mut(&n)
			if g, err := s.MintRunGrant(ctx, tn, "web", p, n); !errors.Is(err, authn.ErrInvalid) {
				t.Errorf("%s: %+v err=%v", name, g, err)
			}
		}
		n := work("task-1", "crm.lookup")
		n.Stack = ""
		if _, err := s.MintRunGrant(ctx, tn, "web", p, n); !errors.Is(err, authn.ErrNoStack) {
			t.Errorf("no app stack: %v", err)
		}
		if _, err := s.MintRunGrant(ctx, "", "web", p, work("task-1", "crm.lookup")); !errors.Is(err, authn.ErrInvalid) {
			t.Errorf("no tenant: %v", err)
		}
	})

	t.Run("a corrupt allowlist is an error, never an empty one", func(t *testing.T) {
		s, tn := newStore(t), tenant()
		p := holder(t, s, tn, "web", "research", "crm.lookup")
		g, err := s.MintRunGrant(ctx, tn, "web", p, work("task-1", "crm.lookup"))
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`[]`, `not json`, `["drive:dc_1"]`, `"crm.lookup"`} {
			if _, err := s.DB.ExecContext(ctx, s.Dialect.Rebind(`UPDATE run_grants SET allowlist = ? WHERE id = ?`), bad, g.ID); err != nil {
				t.Fatal(err)
			}
			if got, err := s.ReadRunGrant(ctx, tn, g.ID); err == nil {
				t.Errorf("allowlist %q read as %+v", bad, got.Allow)
			}
		}
	})

	// Two nodes dispatching the same run at once. Each mint must see the
	// other's, whichever commits first: the newest generation is live and
	// nothing else is. SQLite serializes writers; on Postgres this is the
	// case the run's head row exists for.
	for _, mode := range []string{"the next generation", "generations of the caller's own"} {
		t.Run("concurrent mints of one run leave one live grant: "+mode, func(t *testing.T) {
			s, tn := newStore(t), tenant()
			p := holder(t, s, tn, "web", "research", "crm.lookup")
			const n = 8
			var (
				wg     sync.WaitGroup
				mu     sync.Mutex
				minted = map[int64]string{} // generation → grant id
			)
			for i := 1; i <= n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					in := work("task-1", "crm.lookup")
					if mode != "the next generation" {
						in.Generation = int64(i)
					}
					g, err := s.MintRunGrant(ctx, tn, "web", p, in)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case errors.Is(err, authn.ErrStaleGeneration) && in.Generation != 0:
						// A later generation committed first.
					case err != nil:
						t.Errorf("mint: %v", err)
					case minted[g.Generation] != "":
						t.Errorf("generation %d was minted twice", g.Generation)
					default:
						minted[g.Generation] = g.ID
					}
				}(i)
			}
			wg.Wait()
			if mode == "the next generation" && len(minted) != n {
				t.Errorf("%d of %d mints succeeded", len(minted), n)
			}
			var top int64
			for gen := range minted {
				top = max(top, gen)
			}
			if mode != "the next generation" && top != n {
				t.Errorf("the newest generation is %d, want %d", top, n)
			}
			for gen, id := range minted {
				g, err := s.ReadRunGrant(ctx, tn, id)
				if err != nil {
					t.Fatal(err)
				}
				if live := g.Live(time.Now()); live != (gen == top) {
					t.Errorf("generation %d of %d: live=%v closed=%v reason=%q", gen, top, live, g.ClosedAt, g.CloseReason)
				}
			}
		})
	}
}

func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "svc" + strings.Repeat("x", i%7) + "." + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	return out
}
