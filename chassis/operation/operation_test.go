package operation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

// TestNew Test new operation
func TestNew(t *testing.T) {

	o := New()
	test.Equals(t, true, len(o.OpID) > 0)
	o.OpID = ""
	test.Equals(t, o, &Operation{})
}

// TestCopy Test copy operation
func TestCopy(t *testing.T) {

	o := New()
	id1 := o.OpID
	o2 := o.Copy()
	id2 := o2.OpID

	// fmt.Println(id1, id2)
	// fmt.Println(o.OpID, o2.OpID)
	//
	test.Equals(t, true, len(o2.OpID) > 0)
	test.Equals(t, true, strings.Compare(id1, id2) != 0)
	test.Equals(t, true, strings.Compare(o.OpID, o2.OpID) != 0) // Ids should not be equal
	test.Equals(t, true, strings.Compare(o.Input, o2.Input) == 0) // Input should equal
	test.Equals(t, true, strings.Compare(o.Output, o2.Output) == 0)
}

// TestOperationJSONOmitsSecrets is the load-bearing structural test
// for the secret store's no-leak guarantee at the Operation layer:
// when an op is JSON-encoded (the standard path for trace events,
// continuation snapshots, mock fixtures, debug dumps), the Secrets
// field is omitted entirely — no `"secrets":...` key, no value
// bytes, nothing. The `json:"-"` tag is what enforces this; the
// SecretBag's panicking MarshalJSON is the defense in depth.
func TestOperationJSONOmitsSecrets(t *testing.T) {
	o := New()
	o.Input = `{"foo":"bar"}`
	o.Meta = `{"timeout":1000}`
	o.Secrets.Set("STRIPE_API_KEY", []byte("sk_live_super_secret_abc123"))

	encoded, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// The cleartext bytes must NOT appear anywhere in the output.
	if bytes.Contains(encoded, []byte("sk_live_super_secret_abc123")) {
		t.Errorf("encoded operation leaked cleartext: %s", encoded)
	}
	// The secret name SHOULD be absent too (no `secrets` key at all).
	if bytes.Contains(encoded, []byte("STRIPE_API_KEY")) {
		t.Errorf("encoded operation leaked secret name: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("\"secrets\"")) || bytes.Contains(encoded, []byte("\"Secrets\"")) {
		t.Errorf("encoded operation contains a 'secrets' JSON key: %s", encoded)
	}
	// Sanity: other fields still round-trip (Input + Meta are JSON
	// strings, so their inner braces are escaped — look for the
	// top-level JSON keys instead).
	if !bytes.Contains(encoded, []byte(`"input"`)) || !bytes.Contains(encoded, []byte(`"meta"`)) {
		t.Errorf("encoded operation missing expected non-secret keys: %s", encoded)
	}
}

// TestOperationCopySharesSecretsBag pins the "shared map" semantic
// of Copy(): a copied op and its origin both see the same
// materialized cleartext. The processor's deferred bag.Zero() wipes
// both views in one call (PR 3). If anyone "fixes" Copy() to deep-
// copy the bag, this test fails and forces a deliberate review.
func TestOperationCopySharesSecretsBag(t *testing.T) {
	orig := New()
	orig.Secrets.Set("FOO", []byte("foo-cleartext"))

	copy := orig.Copy()

	// Both see FOO.
	if v, ok := orig.Secrets.Get("FOO"); !ok || string(v) != "foo-cleartext" {
		t.Errorf("orig lost FOO after Copy: (%q, %v)", v, ok)
	}
	if v, ok := copy.Secrets.Get("FOO"); !ok || string(v) != "foo-cleartext" {
		t.Errorf("copy missing FOO: (%q, %v)", v, ok)
	}

	// Set on copy is visible on orig (shared map).
	copy.Secrets.Set("BAR", []byte("bar-cleartext"))
	if _, ok := orig.Secrets.Get("BAR"); !ok {
		t.Errorf("Set on Copy should be visible on origin (shared map)")
	}

	// Zero on copy wipes both views.
	copy.Secrets.Zero()
	if _, ok := orig.Secrets.Get("FOO"); ok {
		t.Errorf("orig still has FOO after copy.Zero — backing map was not shared")
	}
}

// from git repo path to operation structure
func TestPathParse(t *testing.T) {

	tests := []struct {
		input string
		stack string
		scope int
		err   string
	}{
		{
			"APPS/$service/1/readme.md",
			"OPS/$service",
			0,
			"not txcl",
		},
		{
			"OPS/$service/readme.md",
			"OPS/$service",
			0,
			"not txcl",
		},
		{
			"OPS/$boot/0",
			"n/a",
			0,
			"not txcl",
		},
		{
			"OPS/$boot/0/cron/mock-request.json",
			"n/a",
			0,
			"not txcl",
		},
		{
			"OPS/$boot/1/cron/resonator.txcl",
			"$boot",
			1,
			"",
		},
		{
			"OPS/$service/$slot/0000_SETUP/0006_HELLO/resonator.txcl",
			"$service/$slot",
			6,
			"",
		},
		{
			"OPS/$service/1004/resonator.txcl",
			"$service",
			1004,
			"",
		},
		{
			"OPS/$service/$slot/104/resonator.txcl",
			"$service/$slot",
			104,
			"",
		},
		{
			"OPS/$boot/1/dev-repo/resonator.txcl",
			"$boot",
			1,
			"",
		},
	}

	for _, tt := range tests {

		o, err := PathToOperation(tt.input)
		if err == nil {
			test.Equals(t, tt.stack, o.Stack)
		} else {
			test.Equals(t, tt.err, err.Error())
		}
	}
}
