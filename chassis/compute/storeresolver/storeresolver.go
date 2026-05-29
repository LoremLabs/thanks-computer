// Package storeresolver resolves compute refs against the chassis
// content-addressed artifact.Store (the same store snapshots use). A compute
// artifact lives at ref "computes/<alg>/<digest>": the data blob is the wasm
// module, the manifest blob is small JSON naming the engine. This keeps
// compute bytes out of the ops/stack_files tables and reuses the store that is
// already wired into the chassis.
package storeresolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	"github.com/loremlabs/thanks-computer/chassis/compute"
)

// Manifest is the small JSON blob stored alongside the wasm; it records which
// engine runs the artifact.
type Manifest struct {
	Engine string `json:"engine"`
	Alg    string `json:"alg,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// Resolver implements compute.Resolver over an artifact.Store.
type Resolver struct {
	store artifact.Store
}

// New builds a Resolver over the given store.
func New(store artifact.Store) *Resolver { return &Resolver{store: store} }

// Resolve fetches the wasm + engine for a ref. ErrNotFound maps to
// compute.ErrNotFound so callers can distinguish "absent" from "broken".
func (r *Resolver) Resolve(ctx context.Context, ref compute.Ref) (compute.Artifact, error) {
	if r.store == nil {
		return compute.Artifact{}, errors.New("storeresolver: no artifact store configured")
	}
	data, manifest, err := r.store.Get(ctx, ref.StoreRef())
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) {
			return compute.Artifact{}, compute.ErrNotFound
		}
		return compute.Artifact{}, fmt.Errorf("storeresolver: get %s: %w", ref.StoreRef(), err)
	}
	engine := "wazero" // default reference engine if the manifest omits it
	if len(manifest) > 0 {
		var m Manifest
		if json.Unmarshal(manifest, &m) == nil && m.Engine != "" {
			engine = m.Engine
		}
	}
	return compute.Artifact{Alg: ref.Alg, Digest: ref.Digest, Engine: engine, Wasm: data}, nil
}
