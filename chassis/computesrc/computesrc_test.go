package computesrc

import (
	"strings"
	"testing"
)

const digest = "3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2"

func TestPath(t *testing.T) {
	p := Path(digest)
	if p != "COMPUTES/"+digest+".json" {
		t.Fatalf("Path = %q", p)
	}
	if !IsPath(p) || DigestFromPath(p) != digest {
		t.Fatalf("round trip failed for %q", p)
	}
	for _, bad := range []string{
		"COMPUTES/" + strings.ToUpper(digest) + ".json",
		"COMPUTES/" + digest[:63] + ".json",
		"COMPUTES/" + digest + ".js",
		"COMPUTES/x/" + digest + ".json",
		"FILES/" + digest + ".json",
	} {
		if DigestFromPath(bad) != "" {
			t.Errorf("DigestFromPath(%q) should be empty", bad)
		}
	}
	if IsPath("FILES/a.json") || !IsPath("COMPUTES/anything") {
		t.Fatal("IsPath prefix check wrong")
	}
}

func TestEncodeIsCanonical(t *testing.T) {
	a := Bundle{Entry: "plan.js", Files: []File{
		{Path: "plan.js", Content: "export default 1"},
		{Path: "../lib/util.js", Content: "export const x = 1"},
	}}
	b := Bundle{V: 7, Entry: "plan.js", Files: []File{a.Files[1], a.Files[0]}}
	da, ha, err := a.Encode()
	if err != nil {
		t.Fatal(err)
	}
	db, hb, err := b.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || string(da) != string(db) {
		t.Fatalf("file order or V changed the encoding:\n%s\n%s", da, db)
	}
	got, err := Decode(da)
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry != "plan.js" || len(got.Files) != 2 || got.Files[0].Path != "../lib/util.js" {
		t.Fatalf("decode = %+v", got)
	}
}

func TestValidation(t *testing.T) {
	for name, b := range map[string]Bundle{
		"no files":      {Entry: "a.js"},
		"entry missing": {Entry: "a.js", Files: []File{{Path: "b.js"}}},
		"absolute":      {Entry: "/a.js", Files: []File{{Path: "/a.js"}}},
		"unclean":       {Entry: "./a.js", Files: []File{{Path: "./a.js"}}},
		"duplicate":     {Entry: "a.js", Files: []File{{Path: "a.js"}, {Path: "a.js"}}},
		"backslash":     {Entry: `x\a.js`, Files: []File{{Path: `x\a.js`}}},
		"parent only":   {Entry: "..", Files: []File{{Path: ".."}}},
	} {
		if _, _, err := b.Encode(); err == nil {
			t.Errorf("%s: Encode accepted %+v", name, b)
		}
	}
	if _, err := Decode([]byte(`{"v":2,"entry":"a.js","files":[{"path":"a.js","content":""}]}`)); err == nil {
		t.Error("Decode accepted v2")
	}
}
