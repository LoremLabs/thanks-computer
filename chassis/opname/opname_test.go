package opname

import (
	"errors"
	"strings"
	"testing"
)

func TestValidOpName(t *testing.T) {
	// Every rule name shipped in defaults/examples must pass.
	for _, ok := range []string{
		"detect", "healthz", "route", "notfound", "static", "hello",
		"world", "cruel", "research", "sort", "render", "heartbeat",
		"result", "audit", "enrich", "classify", "notify",
		"HELLO", "WORLD", "SORT", "RENDER", "RESEARCH",
		"a", "a-b_c", "Op-1",
	} {
		if err := Valid(ok); err != nil {
			t.Errorf("Valid(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", ".", "..", "a/b", "he%llo", "a b", "a.b", "x\t", "naïve",
		strings.Repeat("a", 65),
	} {
		if err := Valid(bad); err == nil {
			t.Errorf("Valid(%q) = nil, want error", bad)
		} else if !errors.Is(err, ErrName) {
			t.Errorf("Valid(%q) error %v does not wrap ErrName", bad, err)
		}
	}
}

func TestValidStackName(t *testing.T) {
	for _, ok := range []string{
		"hello-world", "txc-continuation", "_sys", "_cron", "boot",
		"_sys/boot", "_sys/txc-continuation", "hello-world/_cron",
		"a", "v2/0100x",
	} {
		if err := ValidStack(ok); err != nil {
			t.Errorf("ValidStack(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", ".", "..", "boot/../x", "/x", "x/", "a//b", "web%",
		"a b", "a.b", "stack/with space", strings.Repeat("s", 129),
	} {
		if err := ValidStack(bad); err == nil {
			t.Errorf("ValidStack(%q) = nil, want error", bad)
		} else if !errors.Is(err, ErrName) {
			t.Errorf("ValidStack(%q) error %v does not wrap ErrName", bad, err)
		}
	}
}
