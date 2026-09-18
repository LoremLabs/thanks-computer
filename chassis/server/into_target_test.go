package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestIntoPath(t *testing.T) {
	cases := []struct {
		into, want string
	}{
		{"", "_kv"},       // not given → the op's default
		{".hits", "hits"}, // the author's own key
		{"_cache.a", "_cache.a"},
		{"@web.res.body", "_txc.web.res.body"}, // author-writable control path
		// reserved → refused in favor of the default, in every spelling sjson
		// resolves to the reserved path
		{"@tenant", "_kv"},
		{"_txc.tenant", "_kv"},
		{`\_txc.tenant`, "_kv"},
		{":_txc.tenant", "_kv"},
		{"@imap.account", "_kv"},
		{"@principal", "_kv"},
		{"@computed.sig_valid", "_kv"}, // a verdict path: only the auth helpers
		{"_txc", "_kv"},
		{`@web.res\`, "_kv"}, // a dangling escape would swallow the appended '.'
	}
	for _, c := range cases {
		meta := []byte(`{}`)
		if c.into != "" {
			meta = []byte(`{"into":` + mustJSON(c.into) + `}`)
		}
		if got := intoPath(meta, "_kv"); got != c.want {
			t.Errorf("intoPath(into=%q) = %q, want %q", c.into, got, c.want)
		}
	}
}

func mustJSON(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return string(append(b, '"'))
}

// TestIntoCannotForgeReservedEndToEnd drives a real op whose `into` is read
// AFTER its side effect: the reserved target is refused, the counter still
// increments exactly once, and the result is reported at the default `_kv`.
func TestIntoCannotForgeReservedEndToEnd(t *testing.T) {
	k := newKVHandle(t)
	for i, into := range []string{"@tenant", ":_txc.fuel_used", `\\_txc.imap.account`} {
		pay, err := callKV(t, kvIncr, k, "t1", "hello", `{"key":"c","into":"`+into+`"}`, "")
		if err != nil {
			t.Fatalf("into=%q: %v", into, err)
		}
		if gjson.Get(pay.Raw, "_txc").Exists() {
			t.Errorf("into=%q wrote under _txc: %s", into, pay.Raw)
		}
		if got := gjson.Get(pay.Raw, "_kv").Int(); got != int64(i+1) {
			t.Errorf("into=%q: result not at the default _kv (want %d): %s", into, i+1, pay.Raw)
		}
	}
}

// TestAuthorChosenTargetsGoThroughTheGuard is a tripwire, not a proof: every
// `txco://` op runs on the trusted transport (output merges unsanitized), so
// an author-chosen output path must be resolved by a guarded helper —
// intoPath here, authorTarget/computedTarget in chassis/ops. A new op that
// reads such a param straight from meta reopens the `into = "@tenant"` forge;
// this fails until it uses the helper (or, if the param is NOT an envelope
// path — like imap/move's mailbox `to` — is listed in notEnvelopePaths).
func TestAuthorChosenTargetsGoThroughTheGuard(t *testing.T) {
	direct := regexp.MustCompile(`gjson\.Get(?:Bytes)?\(\s*\w+,\s*"(into|to|output_path|configured_path|out|dest|target)"\s*\)`)
	helpers := map[string]bool{ // the guarded resolvers themselves
		"readfile.go": true, // intoPath
		"target.go":   true, // authorTarget / computedTarget
	}
	notEnvelopePaths := map[string]bool{
		"imap_ops.go:to":  true, // imap/move: destination MAILBOX name
		"drive_ops.go:to": true, // drive/move, drive/copy: destination DOCUMENT path
	}
	for _, dir := range []string{".", "../ops"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			base := filepath.Base(f)
			if strings.HasSuffix(base, "_test.go") || helpers[base] {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range direct.FindAllStringSubmatch(string(src), -1) {
				if notEnvelopePaths[base+":"+m[1]] {
					continue
				}
				t.Errorf("%s reads WITH %q directly (%s) — resolve it with intoPath / authorTarget / computedTarget so a reserved _txc target is refused", f, m[1], m[0])
			}
		}
	}
}
