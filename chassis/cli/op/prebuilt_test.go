package op

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestBuiltFromWasm(t *testing.T) {
	wasm := []byte("\x00asm\x01\x00\x00\x00fake module bytes")
	b := BuiltFromWasm(wasm)

	sum := sha256.Sum256(wasm)
	wantDigest := hex.EncodeToString(sum[:])
	if b.Digest != wantDigest {
		t.Errorf("Digest = %q, want %q", b.Digest, wantDigest)
	}
	if b.Alg != "sha256" || b.Engine != "wazero" {
		t.Errorf("Alg/Engine = %q/%q, want sha256/wazero", b.Alg, b.Engine)
	}
	if b.Ref != "compute://sha256/"+wantDigest {
		t.Errorf("Ref = %q", b.Ref)
	}
	if string(b.Wasm) != string(wasm) {
		t.Error("Wasm bytes not preserved")
	}
}
