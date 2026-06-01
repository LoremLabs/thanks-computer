package op

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/loremlabs/thanks-computer/chassis/compute"
)

// BuiltFromWasm wraps already-built wasm bytes as a Built, mirroring BuildFile's
// content-addressing (BuildFile dispatch.go:291-297) so a prebuilt <name>.wasm
// shipped in a package yields the SAME compute://sha256/<digest> a local build
// would. No esbuild/javy — the bytes ARE the artifact. Used by `txco apply` when
// a prebuilt wasm sibling is present, so consumers need no toolchain.
func BuiltFromWasm(wasm []byte) Built {
	sum := sha256.Sum256(wasm)
	digest := hex.EncodeToString(sum[:])
	return Built{
		Wasm:   wasm,
		Alg:    "sha256",
		Digest: digest,
		Engine: "wazero",
		Ref:    compute.Ref{Alg: "sha256", Digest: digest}.String(),
	}
}
