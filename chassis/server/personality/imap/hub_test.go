package imap

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/emersion/go-imap/v2/imapserver"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
)

func heads(spec ...string) []chimap.MessageHead {
	// "uid:modseq" or "uid:modseq:flag,flag"
	var out []chimap.MessageHead
	for _, s := range spec {
		var uid uint32
		var ms int64
		var fl string
		n, _ := fmt.Sscanf(s, "%d:%d:%s", &uid, &ms, &fl)
		if n < 2 {
			fmt.Sscanf(s, "%d:%d", &uid, &ms)
		}
		h := chimap.MessageHead{UID: uid, ModSeq: ms}
		if fl != "" {
			h.Flags = []string{fl}
		}
		out = append(out, h)
	}
	return out
}

func render(ups []update) string {
	s := ""
	for i, u := range ups {
		if i > 0 {
			s += " "
		}
		switch u.kind {
		case upExpunge:
			s += fmt.Sprintf("exp%d", u.seq)
		case upExists:
			s += fmt.Sprintf("exists%d", u.n)
		case upFlags:
			s += fmt.Sprintf("flags%d/%d", u.seq, u.uid)
		}
	}
	return s
}

func TestDiff(t *testing.T) {
	cases := []struct {
		name string
		old  []chimap.MessageHead
		new  []chimap.MessageHead
		want string
		ok   bool
	}{
		{"no change", heads("1:1", "2:2"), heads("1:1", "2:2"), "", true},
		{"empty to empty", nil, nil, "", true},
		{"three tail appends", heads("1:1"), heads("1:1", "2:2", "3:3", "4:4"), "exists2 exists3 exists4", true},
		{"first appends into empty", nil, heads("1:1", "2:2"), "exists1 exists2", true},
		{"single middle expunge", heads("1:1", "2:2", "3:3"), heads("1:1", "3:3"), "exp2", true},
		{"several expunges descending", heads("1:1", "2:2", "3:3", "4:4"), heads("2:2"), "exp4 exp3 exp1", true},
		{"flag change at the new seq", heads("1:1", "2:2", "3:3"), heads("1:1", "3:9"), "exp2 flags2/3", true},
		{"interleaved", heads("1:1", "2:2", "3:3", "4:4"), heads("2:2", "4:9", "5:5", "6:6"), "exp3 exp1 exists3 exists4 flags2/4", true},
		{"everything expunged", heads("1:1", "2:2"), nil, "exp2 exp1", true},
		{"replace = expunge + append", heads("1:1", "2:2"), heads("2:2", "3:3"), "exp1 exists2", true},
		{"non-tail uid", heads("2:2", "5:5"), heads("2:2", "3:3", "5:5"), "", false},
		{"lower uid after all old gone", heads("5:5"), heads("1:1"), "", false},
	}
	for _, c := range cases {
		ups, ok := diff(c.old, c.new)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if got := render(ups); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// Randomised: diff+apply against a real tracker must never panic and must
// keep the tracker's count equal to the model, observable through a fresh
// session's EncodeSeqNum (n is visible, n+1 is not).
func TestApplyKeepsTrackerCount(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var model []chimap.MessageHead
	var nextUID uint32 = 1
	var modseq int64
	st := &mailboxState{tracker: imapserver.NewMailboxTracker(0), suppress: map[uint32]*imapserver.SessionTracker{}}
	check := func(step int) {
		t.Helper()
		sess := st.tracker.NewSession()
		defer sess.Close()
		n := uint32(len(model))
		if n > 0 && sess.EncodeSeqNum(n) == 0 {
			t.Fatalf("step %d: seq %d should be visible", step, n)
		}
		if sess.EncodeSeqNum(n+1) != 0 {
			t.Fatalf("step %d: seq %d should not exist (count drifted)", step, n+1)
		}
	}
	for step := 0; step < 200; step++ {
		old := copyHeads(model)
		switch r := rng.Intn(10); {
		case r < 4 || len(model) == 0: // append 1-3
			for k := rng.Intn(3) + 1; k > 0; k-- {
				modseq++
				model = append(model, chimap.MessageHead{UID: nextUID, ModSeq: modseq})
				nextUID++
			}
		case r < 7: // expunge 1-2 random rows
			for k := rng.Intn(2) + 1; k > 0 && len(model) > 0; k-- {
				i := rng.Intn(len(model))
				model = append(model[:i:i], model[i+1:]...)
			}
		default: // flag change
			i := rng.Intn(len(model))
			modseq++
			model[i].ModSeq = modseq
			model[i].Flags = []string{`\Seen`}
		}
		ups, ok := diff(old, model)
		if !ok {
			t.Fatalf("step %d: diff refused a monotonic change", step)
		}
		st.apply(ups)
		check(step)
	}
	// Empty the mailbox entirely, then refill: no EXISTS 0, no panic.
	old := copyHeads(model)
	model = nil
	ups, _ := diff(old, model)
	for _, u := range ups {
		if u.kind == upExists {
			t.Fatal("EXISTS emitted for an emptied mailbox")
		}
	}
	st.apply(ups)
	check(9998)
	old = nil
	model = heads(fmt.Sprintf("%d:%d", nextUID, modseq+1))
	ups, _ = diff(old, model)
	st.apply(ups)
	check(9999)
}
