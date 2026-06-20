package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRunRoomUsage(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		var out, errb bytes.Buffer
		if code := runRoom([]string{arg}, &out, &errb); code != 0 {
			t.Fatalf("runRoom %q exit = %d, want 0", arg, code)
		}
		if !strings.Contains(out.String(), "Usage:") {
			t.Fatalf("runRoom %q: missing usage; got %q", arg, out.String())
		}
	}
}

func TestRunRoomNoMessageNonTTY(t *testing.T) {
	// No message + non-terminal stdin → a usage error, not a hung interactive
	// read. Point stdin at a pipe so the test is deterministic regardless of
	// how `go test` was launched.
	rp, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rp.Close()
	old := os.Stdin
	os.Stdin = rp
	defer func() { os.Stdin = old }()

	var out, errb bytes.Buffer
	code := runRoom([]string{"--room", "support"}, &out, &errb)
	if code != 2 {
		t.Fatalf("runRoom no-message exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "terminal") {
		t.Fatalf("expected a non-terminal notice on stderr, got %q", errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected no stdout, got %q", out.String())
	}
}
