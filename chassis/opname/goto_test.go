package opname

import "testing"

func TestGotoTarget(t *testing.T) {
	ok := map[string]string{
		"goto://billing/100":        "billing/100",
		"goto://300":                "300",
		"goto://0":                  "0",
		"goto://website/canary/0":   "website/canary/0", // a slot is a multi-segment stack
		"goto://node/_websocket/10": "node/_websocket/10",
		"goto://_sys/boot/100":      "_sys/boot/100", // naming a stack is not reaching it: routing decides
	}
	for in, want := range ok {
		got, err := GotoTarget(in)
		if err != nil || got != want {
			t.Errorf("GotoTarget(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := []string{
		"goto://",                 // no target
		"goto://billing",          // a stack with no scope
		"goto://billing/",         // an empty scope
		"goto:///100",             // an empty stack
		"goto://billing//100",     // an empty segment
		"goto://bill ing/1",       // whitespace
		"goto://../etc/1",         // a traversal segment
		`goto://a","halt":true/1`, // quotes cannot reach the JSON the jump is written into
		"goto://billing/1x",       // not a number
		"goto://billing/-1",       // not a number
		"goto://1234567890",       // over nine digits
		"goto:// 100",             // whitespace
		"billing/100",             // no scheme: the unschemed form is not this function's
	}
	for _, in := range bad {
		if got, err := GotoTarget(in); err == nil {
			t.Errorf("GotoTarget(%q) = %q, nil; want an error", in, got)
		}
	}
}
