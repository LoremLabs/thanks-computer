package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var updateFlagsDoc = flag.Bool("update-flags-doc", false, "regenerate docs/advanced/flags.md from the Config struct tags")

// flagsDocPath is the generated flag reference, relative to this package.
var flagsDocPath = filepath.Join("..", "..", "docs", "advanced", "flags.md")

// renderFlagsDoc writes every `txco serve` flag, in struct order, from the same
// `id` / `default` / `desc` tags loadFromFlagsAndEnv registers them from, so the
// page cannot describe a flag the chassis does not have, or miss one it does.
func renderFlagsDoc() []byte {
	var b bytes.Buffer
	b.WriteString(`<!-- Generated from chassis/config/config.go — do not edit by hand.
     Regenerate: go test ./chassis/config -run TestFlagsDocIsCurrent -update-flags-doc -->

# Every ` + "`txco serve`" + ` flag

Each flag can also be set from the environment as ` + "`TXCO_<FLAG>`" + `, upper case with
dashes as underscores (` + "`--trace-mode`" + ` is ` + "`TXCO_TRACE_MODE`" + `). A flag on the command line
wins over the environment, which wins over the default. ` + "`txco serve --help`" + ` prints
the same list. What the main ones mean, together: [the runtime reference](./serve.md).

| Flag | Default | What it does |
|---|---|---|
`)
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		id := f.Tag.Get("id")
		if id == "" {
			continue
		}
		def := f.Tag.Get("default")
		if def == "" {
			def = "—"
		} else {
			def = "`" + def + "`"
		}
		desc := strings.ReplaceAll(f.Tag.Get("desc"), "|", `\|`)
		fmt.Fprintf(&b, "| `--%s` | %s | %s |\n", id, strings.ReplaceAll(def, "|", `\|`), desc)
	}
	return b.Bytes()
}

// TestFlagsDocIsCurrent keeps docs/advanced/flags.md in step with the Config
// struct: adding, renaming or re-describing a flag fails here until the page
// is regenerated with -update-flags-doc.
func TestFlagsDocIsCurrent(t *testing.T) {
	want := renderFlagsDoc()
	if *updateFlagsDoc {
		if err := os.WriteFile(flagsDocPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(flagsDocPath)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with -update-flags-doc)", flagsDocPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with config.go; regenerate: go test ./chassis/config -run TestFlagsDocIsCurrent -update-flags-doc", flagsDocPath)
	}
}
