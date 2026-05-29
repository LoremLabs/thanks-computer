package auth

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// PrintCLIError writes a terminal error line in the canonical CLI
// shape: red text when stderr is a TTY, plain otherwise, followed by
// one blank line so the next shell prompt isn't glued to the message.
//
// No trailing `\t`. An earlier convention used `\n\n\t` to indent the
// shell prompt one tab, but on zsh the missing trailing newline
// triggers the PROMPT_SP indicator (a reverse-`%` mark) — visually
// noisier than the indent was worth. The blank line alone does the
// job.
//
// Pass a pre-formatted message; callers do their own fmt.Sprintf if
// they need to interpolate, or use PrintCLIErrorf below.
func PrintCLIError(stderr io.Writer, message string) {
	if isStderrTTY(stderr) {
		// ANSI SGR 31 = red, 0 = reset. Wrap only the message body,
		// not the trailing newlines — embedding ANSI in newlines
		// occasionally confuses log scrapers.
		_, _ = io.WriteString(stderr, "\033[31m"+message+"\033[0m\n\n")
		return
	}
	_, _ = io.WriteString(stderr, message+"\n\n")
}

// PrintCLIErrorf is the fmt.Sprintf-style sibling of PrintCLIError.
// Matches the format-string shape callers are used to from
// `fmt.Fprintf(stderr, "verb: %v\n\n\t", err)`.
func PrintCLIErrorf(stderr io.Writer, format string, args ...any) {
	PrintCLIError(stderr, fmt.Sprintf(format, args...))
}

// isStderrTTY reports whether the writer is a terminal we can decorate
// with ANSI codes. Returns false for *bytes.Buffer (tests), pipes,
// files, or anything that isn't an *os.File.
//
// NO_COLOR (https://no-color.org/) is respected: setting it to ANY
// non-empty value disables color regardless of TTY status.
func isStderrTTY(stderr io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := stderr.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
