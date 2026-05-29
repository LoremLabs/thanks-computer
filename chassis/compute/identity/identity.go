// Package identity is a reference compute engine that echoes its input back
// as output. It runs no sandbox and ignores the artifact bytes — it exists to
// exercise the compute seam end to end (dispatch → resolve → engine → merge)
// without a wasm toolchain. Real engines (e.g. compute/wazero) self-register
// the same way. Activate it with a blank import.
package identity

import (
	"context"

	"github.com/loremlabs/thanks-computer/chassis/compute"
)

// Name is the registered engine identifier.
const Name = "identity"

func init() {
	compute.RegisterEngine(Name, func(compute.EngineConfig) (compute.Engine, error) {
		return engine{}, nil
	})
}

type engine struct{}

func (engine) Name() string { return Name }

func (engine) Load(context.Context, compute.Artifact) (compute.Instance, error) {
	return instance{}, nil
}

type instance struct{}

// Invoke echoes the input envelope. Empty input becomes "{}" so the merge
// downstream always sees valid JSON.
func (instance) Invoke(_ context.Context, input []byte, _ compute.Limits) ([]byte, error) {
	if len(input) == 0 {
		return []byte("{}"), nil
	}
	return input, nil
}

func (instance) Close(context.Context) error { return nil }
