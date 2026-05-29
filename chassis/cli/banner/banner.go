// Package banner holds the txco logo + small TTY helpers used by
// every subcommand's help/usage screen. Lives in its own package so
// both chassis/cli and chassis/cli/auth can import it without a cycle
// (chassis/cli imports chassis/cli/auth for dispatch).
package banner

import (
	"fmt"
	"io"
	"os"
)

// PrintLogo writes the txco banner to w. The three dots colorize
// (cyan/magenta/yellow) when w is a terminal; piped output stays
// plain so it doesn't leak escape codes into log files or pagers.
func PrintLogo(w io.Writer) {
	cyan, magenta, yellow, reset := "", "", "", ""
	if IsTTY(w) {
		cyan = "\x1b[36m"
		magenta = "\x1b[35m"
		yellow = "\x1b[33m"
		reset = "\x1b[0m"
	}
	co := cyan + "o" + reset
	mo := magenta + "o" + reset
	yo := yellow + "o" + reset

	fmt.Fprintf(w, `┌─────────────────┐
│     thanks,     │
└─────────────────┘
      /  |  \
     %s   %s   %s
      \  |  /
┌─────────────────┐
│    computer.    │
└─────────────────┘
`, co, mo, yo)
}

// IsTTY reports whether w wraps a character device (i.e. an
// interactive terminal). Returns false for pipes, files, and any
// non-*os.File writer. No external dep required — ModeCharDevice on
// the FileInfo is the same check mattn/go-isatty performs on
// darwin/linux.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
