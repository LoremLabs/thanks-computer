package runtime

import "github.com/tidwall/gjson"

// Env is the resolution context used by Resolve when it walks a
// PathRef node. Decoupling the evaluator from a specific backing
// representation keeps the door open for in-memory sub-envelopes
// (e.g. a parsed value held in a variable after `&json(...)`)
// without changing Resolve's signature.
//
// Implementations supplied here:
//   - JSONEnv  — the standard chassis envelope (JSON string,
//                gjson-backed). Used by the processor.
//   - MapEnv   — a flat-key test helper. Used by runtime tests
//                that don't want to construct real JSON.
type Env interface {
	Get(path string) (any, bool)
}

// JSONEnv wraps a JSON-string envelope. Path lookups go through
// gjson; semantics match what every existing envelope read in the
// chassis does today. A missing path returns (nil, false).
type JSONEnv string

func (e JSONEnv) Get(path string) (any, bool) {
	r := gjson.Get(string(e), path)
	if !r.Exists() {
		return nil, false
	}
	return r.Value(), true
}

// MapEnv is a test helper. Reads flat-key (top-level only)
// values from an in-memory map. Tests that need nested-path
// behavior should pre-flatten the map or use JSONEnv with a
// real JSON string.
type MapEnv map[string]any

func (m MapEnv) Get(path string) (any, bool) {
	v, ok := m[path]
	return v, ok
}

// MultiEnv tries each Env in order; first match wins. Used to expose
// multiple envelope sources at the same resolution boundary.
//
// Concrete use: the processor's EMIT overlay needs to see both the
// EXEC's fresh output (so `EMIT @reply = .text` projects the handler's
// reply) AND the scope input envelope (so `EMIT @reply = @web.req.body`
// can still reach the input). Composing them as a MultiEnv lets either
// path-shape work without the caller pre-merging two large JSON docs.
//
// A nil or empty MultiEnv returns (nil, false) for every Get.
type MultiEnv []Env

func (m MultiEnv) Get(path string) (any, bool) {
	for _, e := range m {
		if e == nil {
			continue
		}
		if v, ok := e.Get(path); ok {
			return v, ok
		}
	}
	return nil, false
}
