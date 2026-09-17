package drive_test

import (
	"context"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/drive"
)

func TestPolicyAllows(t *testing.T) {
	// The shape a curated drive uses: the stack owns Knowledge/, so a
	// client may rearrange and delete there but not write bytes; one
	// folder inside it is handed back.
	p, err := drive.ParsePolicy(`{
		"Knowledge": {"write": "deny"},
		"Knowledge/scratch": {"write": "allow"},
		"Vault": {"write": "deny", "delete": "deny", "move_out": "deny"}
	}`)
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	for _, c := range []struct {
		path, verb string
		want       bool
		why        string
	}{
		{"", drive.VerbWrite, true, "the root is not named"},
		{"drop/a.pdf", drive.VerbWrite, true, "an unnamed subtree allows"},
		{"Knowledge", drive.VerbWrite, false, "the named prefix itself"},
		{"Knowledge/a.pdf", drive.VerbWrite, false, "inherits from the prefix"},
		{"Knowledge/deep/er/a.pdf", drive.VerbWrite, false, "inherits at any depth"},
		{"Knowledge/a.pdf", drive.VerbDelete, true, "an unnamed verb allows"},
		{"Knowledge/a.pdf", drive.VerbMoveIn, true, "filing into it is still allowed"},
		{"Knowledge/scratch/a.pdf", drive.VerbWrite, true, "the LONGER prefix wins and readmits"},
		{"Knowledgeable/a.pdf", drive.VerbWrite, true, "a name that merely starts the same is outside"},
		{"Vault/x", drive.VerbDelete, false, "a second prefix carries its own verbs"},
		{"Vault/x", drive.VerbMoveOut, false, "move_out is refused at the source"},
		{"Vault/x", drive.VerbCreate, true, "create was not named"},
	} {
		if got := p.Allows(c.path, c.verb); got != c.want {
			t.Errorf("Allows(%q, %q) = %v, want %v — %s", c.path, c.verb, got, c.want, c.why)
		}
	}

	// No policy is an ordinary drive.
	var none drive.Policy
	if !none.Allows("Knowledge/a", drive.VerbWrite) {
		t.Error("the zero policy must allow everything")
	}
	if none.String() != "" {
		t.Errorf("the zero policy serializes to %q, want empty", none.String())
	}

	// A round trip through storage keeps the meaning.
	back, err := drive.ParsePolicy(p.String())
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back.Allows("Knowledge/a.pdf", drive.VerbWrite) || !back.Allows("Knowledge/scratch/a.pdf", drive.VerbWrite) {
		t.Error("round trip changed the meaning")
	}
}

func TestPolicyValidateRefusesTypos(t *testing.T) {
	// A typo must not fail OPEN: an unknown verb or mode would otherwise
	// allow exactly what the author meant to refuse.
	for _, raw := range []string{
		`{"Knowledge": {"put": "deny"}}`,
		`{"Knowledge": {"write": "refuse"}}`,
		`{"../escape": {"write": "deny"}}`,
	} {
		if _, err := drive.ParsePolicy(raw); err == nil {
			t.Errorf("ParsePolicy(%s) accepted a bad policy", raw)
		}
	}
	// An empty policy is not an error, it is simply no policy.
	for _, raw := range []string{"", "  ", "{}", "null"} {
		p, err := drive.ParsePolicy(raw)
		if err != nil || len(p) != 0 {
			t.Errorf("ParsePolicy(%q) = %v, %v; want empty, nil", raw, p, err)
		}
	}
	if err := (drive.Policy{"ok": {drive.VerbWrite: drive.ModeDeny}}).Validate(); err != nil {
		t.Errorf("a good policy was refused: %v", err)
	}
	if got := strings.Join(drive.PolicyVerbs(), ","); got != "create,delete,move_in,move_out,write" {
		t.Errorf("PolicyVerbs() = %s", got)
	}
}

func TestCollectionPolicyRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := newColl(t, s)
	if len(c.Policy) != 0 {
		t.Fatalf("a new collection starts with a policy: %v", c.Policy)
	}
	want := drive.Policy{"Knowledge": {drive.VerbWrite: drive.ModeDeny}}
	got, err := s.SetCollectionPolicy(ctx, "tnt_a", "docs", want)
	if err != nil {
		t.Fatalf("SetCollectionPolicy: %v", err)
	}
	if got.Policy.Allows("Knowledge/a", drive.VerbWrite) {
		t.Error("the policy did not take")
	}
	// It survives a re-read, which is what the head does on every login.
	reread, found, err := s.GetCollection(ctx, "tnt_a", "docs")
	if err != nil || !found {
		t.Fatalf("GetCollection: %v found=%v", err, found)
	}
	if reread.Policy.Allows("Knowledge/a", drive.VerbWrite) {
		t.Error("the policy did not survive storage")
	}
	// Clearing it restores an ordinary drive.
	cleared, err := s.SetCollectionPolicy(ctx, "tnt_a", "docs", nil)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !cleared.Policy.Allows("Knowledge/a", drive.VerbWrite) {
		t.Error("clearing the policy left it in force")
	}
	if _, err := s.SetCollectionPolicy(ctx, "tnt_a", "nope", want); err == nil {
		t.Error("setting a policy on a missing collection succeeded")
	}
}
