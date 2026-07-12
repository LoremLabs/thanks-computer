package jsonx

import (
	"testing"
)

// FuzzBuilder decodes arbitrary bytes into a valid op tape (indices
// into the same pools as the random test) so the fuzzer explores op
// orderings and pool combinations with real signal.
func FuzzBuilder(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{9, 40, 3, 17, 22, 8, 8, 8})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, tape []byte) {
		start := 0
		if len(tape) > 0 {
			start = int(tape[0]) % 3
		}
		var ops []bop
		for i := 1; i+2 < len(tape) && len(ops) < 24; i += 3 {
			path := builderPathPool[int(tape[i])%len(builderPathPool)]
			if tape[i+1]&3 == 0 {
				ops = append(ops, bop{raw: true, path: path,
					rawV: builderRawPool[int(tape[i+2])%len(builderRawPool)]})
			} else {
				ops = append(ops, bop{path: path,
					val: builderValPool[int(tape[i+2])%len(builderValPool)]})
			}
		}
		want := applyChain(start, ops)
		got := applyBuilder(start, ops)
		if got != want {
			t.Fatalf("mismatch (start=%d)\nops:  %+v\nsjson: %q\njsonx: %q",
				start, ops, want, got)
		}
	})
}
