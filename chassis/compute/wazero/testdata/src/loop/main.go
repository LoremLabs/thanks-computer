// loop is a runaway-guest fixture: after draining stdin it spins forever. The
// engine's wall-clock limit (CloseOnContextDone) must kill it. Built to wasm
// (GOOS=wasip1) at test time; not part of normal ./... builds.
package main

import (
	"io"
	"os"
)

func main() {
	_, _ = io.ReadAll(os.Stdin)
	x := 0
	for {
		x++
		if x < 0 { // never true; defeats dead-loop elimination
			break
		}
	}
}
