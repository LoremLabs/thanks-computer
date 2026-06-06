package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestMailParent(t *testing.T) {
	cases := []struct {
		stack  string
		parent string
		isMail bool
	}{
		{"_mail", "", true},                // bare tenant mail stack
		{"test-01/_mail", "test-01", true}, // nested under an HTTP stack
		{"a/b/_mail", "a/b", true},         // deeper nesting
		{"web", "", false},                 // ordinary HTTP stack
		{"_sys/boot", "", false},           // not a mail stack
		{"mailbox", "", false},             // contains "mail" but isn't _mail
	}
	for _, c := range cases {
		p, m := mailParent(c.stack)
		if m != c.isMail || p != c.parent {
			t.Errorf("mailParent(%q) = (%q,%v), want (%q,%v)", c.stack, p, m, c.parent, c.isMail)
		}
	}
}

func TestPrintDriftTableWithMailPath(t *testing.T) {
	drifts := []stackDrift{
		{Stack: "test-01/_mail", Remote: "v1", Local: "v1 (clean)", Note: "in sync",
			MailPath: "*@acme.com"},
	}
	var buf bytes.Buffer
	printDriftTable(&buf, drifts) // not a TTY → no ANSI
	out := buf.String()
	if !strings.Contains(out, "mail=*@acme.com") {
		t.Errorf("expected mail= cell for the _mail stack:\n%s", out)
	}
	if !strings.Contains(out, "→ in sync") {
		t.Errorf("note column missing:\n%s", out)
	}
}
