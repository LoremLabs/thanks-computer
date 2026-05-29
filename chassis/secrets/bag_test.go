package secrets

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestSecretBagZeroValueIsUsable(t *testing.T) {
	var b SecretBag

	if v, ok := b.Get("MISSING"); v != nil || ok {
		t.Errorf("zero-value Get: got (%v, %v), want (nil, false)", v, ok)
	}
	if n := b.Names(); n != nil {
		t.Errorf("zero-value Names: got %v, want nil", n)
	}
	if l := b.Len(); l != 0 {
		t.Errorf("zero-value Len: got %d, want 0", l)
	}
	b.Zero() // must not panic on zero value
}

func TestSecretBagSetGet(t *testing.T) {
	var b SecretBag
	b.Set("STRIPE", []byte("sk_live_abc"))
	b.Set("SLACK", []byte("xoxb-def"))

	if v, ok := b.Get("STRIPE"); !ok || string(v) != "sk_live_abc" {
		t.Errorf("Get STRIPE: got (%q, %v)", v, ok)
	}
	if v, ok := b.Get("SLACK"); !ok || string(v) != "xoxb-def" {
		t.Errorf("Get SLACK: got (%q, %v)", v, ok)
	}
	if _, ok := b.Get("UNKNOWN"); ok {
		t.Errorf("Get UNKNOWN: got ok=true, want false")
	}
	if l := b.Len(); l != 2 {
		t.Errorf("Len: got %d, want 2", l)
	}
}

func TestSecretBagNamesIsSorted(t *testing.T) {
	var b SecretBag
	b.Set("ZZZ", []byte("z"))
	b.Set("AAA", []byte("a"))
	b.Set("MMM", []byte("m"))

	got := b.Names()
	want := []string{"AAA", "MMM", "ZZZ"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names: got %v, want %v", got, want)
	}
}

func TestSecretBagZeroWipesValues(t *testing.T) {
	var b SecretBag
	original := []byte("super-secret-key")
	stored := make([]byte, len(original))
	copy(stored, original)
	b.Set("FOO", stored)

	b.Zero()

	// After Zero, the bag is empty.
	if l := b.Len(); l != 0 {
		t.Errorf("after Zero, Len = %d, want 0", l)
	}
	if _, ok := b.Get("FOO"); ok {
		t.Errorf("after Zero, Get(FOO) should be (nil, false)")
	}
	// The original backing slice is zeroed in place. We can verify
	// because Zero(v) operates on the slice we handed in.
	for i, byteVal := range stored {
		if byteVal != 0 {
			t.Errorf("backing byte at %d not zeroed: %v", i, byteVal)
		}
	}
}

func TestSecretBagZeroIdempotent(t *testing.T) {
	var b SecretBag
	b.Set("X", []byte("x"))
	b.Zero()
	b.Zero() // must not panic on already-zeroed bag

	if l := b.Len(); l != 0 {
		t.Errorf("after double Zero, Len = %d, want 0", l)
	}
}

// TestSecretBagJSONMarshalPanics is the load-bearing structural test
// for the whole design: if this ever stops panicking, the
// non-serializability invariant is broken and PR 3's no-leak guarantee
// collapses.
func TestSecretBagJSONMarshalPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("json.Marshal(SecretBag) did not panic — non-serializability invariant broken")
		}
	}()
	var b SecretBag
	b.Set("STRIPE", []byte("sk_live_abc"))
	_, _ = json.Marshal(b)
}

func TestSecretBagJSONMarshalEmptyAlsoPanics(t *testing.T) {
	// Even a zero-value bag must panic — defense in depth: never
	// accidentally produce `{}` either.
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("json.Marshal(empty SecretBag) did not panic")
		}
	}()
	var b SecretBag
	_, _ = json.Marshal(b)
}

func TestSecretBagTextMarshalPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("MarshalText did not panic")
		}
	}()
	var b SecretBag
	_, _ = b.MarshalText()
}

func TestSecretBagGobEncodePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("gob.NewEncoder.Encode(SecretBag) did not panic")
		}
	}()
	var buf bytes.Buffer
	_ = gob.NewEncoder(&buf).Encode(SecretBag{})
}

// TestSecretBagNoStringerImpl is a canary: SecretBag must NOT
// implement fmt.Stringer (or any human-readable formatter). The
// reasoning is defense in depth: the bag is full of byte-slices that
// %v already renders as int lists — ugly enough to discourage being
// pasted into logs, conspicuous enough to catch in review. A
// Stringer that returned, say, the name list would "fix" the
// ugliness and quietly normalize accidental %v leaks. If anyone ever
// adds one to "clean up dev output", this test fails and forces a
// deliberate review.
func TestSecretBagNoStringerImpl(t *testing.T) {
	var b SecretBag
	if _, ok := any(b).(fmt.Stringer); ok {
		t.Errorf("SecretBag implements fmt.Stringer — that normalizes %%v leaks; remove the String() method")
	}
	if _, ok := any(&b).(fmt.Stringer); ok {
		t.Errorf("*SecretBag implements fmt.Stringer — same hazard as above")
	}
}

func TestSecretBagCopySharedMap(t *testing.T) {
	// A value-copy of SecretBag gives a new struct with the same map
	// pointer. Setting on the copy affects the "original" — this is
	// the intended semantic for Operation.Copy() within a single
	// request scope.
	var b SecretBag
	b.Set("ORIG", []byte("v1"))

	c := b // value copy
	c.Set("FROM_COPY", []byte("v2"))

	if _, ok := b.Get("FROM_COPY"); !ok {
		t.Errorf("Set on copy should be visible on original (shared map)")
	}
}
