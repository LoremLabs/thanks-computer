package secrets

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDeriveKey(t *testing.T) {
	mkA, err := NewInlineMasterKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	mkB, _ := NewInlineMasterKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))

	k1, ver, err := DeriveKey(mkA, "txco/test/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 || ver != mkA.Version() {
		t.Fatalf("len=%d ver=%d", len(k1), ver)
	}
	// Deterministic: every node holding the same master key derives the same
	// subkey — the whole point.
	k1again, _, _ := DeriveKey(mkA, "txco/test/v1")
	if !bytes.Equal(k1, k1again) {
		t.Error("same key + label derived two different subkeys")
	}
	// Separated by label and by master key, and never the master key itself.
	k2, _, _ := DeriveKey(mkA, "txco/test/v2")
	k3, _, _ := DeriveKey(mkB, "txco/test/v1")
	if bytes.Equal(k1, k2) || bytes.Equal(k1, k3) || bytes.Equal(k1, mkA.Key()) {
		t.Error("subkeys are not domain-separated")
	}
	if _, _, err := DeriveKey(nil, "x"); err == nil {
		t.Error("nil provider accepted")
	}
	if _, _, err := DeriveKey(mkA, ""); err == nil {
		t.Error("empty label accepted")
	}
}
