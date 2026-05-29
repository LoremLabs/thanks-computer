// transform is a WASI-command compute fixture: it reads a JSON object from
// stdin, sets "computed":true, and writes the result to stdout. A line on
// stderr exercises the log channel. Built to wasm (GOOS=wasip1) at test time;
// not part of normal ./... builds (it lives under testdata).
package main

import (
	"encoding/json"
	"io"
	"os"
)

func main() {
	in, _ := io.ReadAll(os.Stdin)
	m := map[string]any{}
	if len(in) > 0 {
		_ = json.Unmarshal(in, &m)
	}
	m["computed"] = true
	out, _ := json.Marshal(m)
	_, _ = os.Stdout.Write(out)
	_, _ = os.Stderr.Write([]byte("transform ran\n"))
}
