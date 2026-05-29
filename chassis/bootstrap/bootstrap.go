package bootstrap

import (
	"hash/fnv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/runtime"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	// "github.com/savaki/jq"
)

// normalizeSelectPath converts txcl `@foo` / `.foo` path sugar to the
// dotted form gjson/sjson expect. Mirrors processor.normalizeSelectPath
// (kept local to avoid a bootstrap→processor dependency).
func normalizeSelectPath(p string) string {
	if strings.HasPrefix(p, "@") {
		return "_txc." + strings.TrimPrefix(p, "@")
	}
	return strings.TrimPrefix(p, ".")
}

// Note on error handling: runtime.Resolve errors are swallowed in
// this evaluator because Eval has no error return and is only
// exercised from tests (no production caller). Today only ast.Literal
// can appear, and Literals never error. When FunctionCall lands
// (PR 3+) and bootstrap.Eval starts seeing it, this function should
// gain an error return and propagate — track via the implementation
// plan.

// pick the best resonator from our collection for a given event
func Eval(input string, resonators []*resonator.Resonator, hashSeed string) (*resonator.Resonator, string) {

	var best *resonator.Resonator = nil

	/*
	 * Order: WHEN, HAVING, PRE-SET, SELECT, POST-SET, WITH, PRIORITY, EXEC
	 *
	 * Only process HAVING if WHEN matches
	 * ..only process PRE-SET if HAVING matches
	 *
	 * PRE-SET, SELECT, POST-SET control output event
	 * WITH is control meta-data
	 * PRIORITY orders the resonators
	 * EXEC is the location to execute the resonator runs
	 */

	// short circuit, no resonators
	if len(resonators) == 0 {
		return nil, input
	}

	// get matches
	i := 0
	highestPriority := int64(0)
	for _, res := range resonators {
		if res.WhenMatches(input) {
			resonators[i] = res
			i++

			if res.Priority > highestPriority {
				highestPriority = res.Priority
			}
		}
	}
	if i != 0 && len(resonators) != 0 {
		resonators = resonators[:i]
	} else {
		return nil, input
	}

	// choose resonator with the highest priority
	i = 0
	for _, res := range resonators {
		if res.Priority == highestPriority {
			// fmt.Printf("highest %d %d", highestPriority, res.Priority)
			resonators[i] = res
			i++
		}
	}
	if i != 0 && len(resonators) != 0 {
		resonators = resonators[:i]
	}

	// if we have more than one match, use hashSeed(RID?) to pick
	if len(resonators) > 1 && len(hashSeed) != 0 {
		h := fnv.New32a()
		h.Write([]byte(hashSeed)) // nolint: errcheck
		shuffle := h.Sum32()

		best = resonators[shuffle%uint32(len(resonators))]
	} else {
		best = resonators[0]
	}

	// decorate the event
	// PRE-SET
	// debug, _ := json.MarshalIndent(best, "", " ")
	// fmt.Printf("resonator %s\n\n", debug)

	if best.SetPre != nil {
		// for _, override := range best.SetPre.Overrides {
		// 	branch := strings.TrimPrefix(override.Path, ".")
		// 	val := override.Value
		// 	input.Obj.SetP(val, branch)
		// }
		for _, override := range best.SetPre.Overrides {
			branch := strings.TrimPrefix(override.Path, ".")
			val, _ := runtime.Resolve(override.Value, runtime.JSONEnv(input))
			altered, _ := sjson.Set(input, branch, val)
			input = altered
		}
	}

	// SELECT (path-copy with optional DEFAULT). Applies each
	// `<src> AS <dst> [DEFAULT <lit>]` to the input envelope.
	if best.Select != nil {
		for _, asn := range best.Select.Assignments {
			srcPath := normalizeSelectPath(asn.From)
			dstPath := normalizeSelectPath(asn.To)
			val := gjson.Get(input, srcPath)
			useDefault := !val.Exists() ||
				(val.Type == gjson.String && val.String() == "")
			var (
				goVal  interface{}
				rawVal string
			)
			if useDefault && asn.HasDefault {
				goVal, _ = runtime.Resolve(asn.Default, runtime.JSONEnv(input))
			} else if useDefault {
				goVal = ""
			} else {
				goVal = val.Value()
				rawVal = val.Raw
			}
			if rawVal != "" {
				if altered, err := sjson.SetRaw(input, dstPath, rawVal); err == nil {
					input = altered
				}
			} else {
				if altered, err := sjson.Set(input, dstPath, goVal); err == nil {
					input = altered
				}
			}
		}
	}

	// POST-SET
	if best.SetPost != nil {
		for _, override := range best.SetPost.Overrides {
			branch := strings.TrimPrefix(override.Path, ".")
			val, _ := runtime.Resolve(override.Value, runtime.JSONEnv(input))
			altered, _ := sjson.Set(input, branch, val)
			input = altered
		}
	}

	// NB: meta-data in best.With
	return best, input
}
