package stackdir

import (
	"archive/tar"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const digest = "3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2"

func TestPath(t *testing.T) {
	p := Path(digest)
	if p != "STACKDIR/"+digest+".tar" {
		t.Fatalf("Path = %q", p)
	}
	if !IsPath(p) || DigestFromPath(p) != digest || !ValidDigest(digest) {
		t.Fatalf("round trip failed for %q", p)
	}
	for _, bad := range []string{
		"STACKDIR/" + strings.ToUpper(digest) + ".tar",
		"STACKDIR/" + digest[:63] + ".tar",
		"STACKDIR/" + digest + ".json",
		"STACKDIR/x/" + digest + ".tar",
		"COMPUTES/" + digest + ".tar",
	} {
		if DigestFromPath(bad) != "" {
			t.Errorf("DigestFromPath(%q) should be empty", bad)
		}
	}
	if IsPath("FILES/a.tar") || !IsPath("STACKDIR/anything") {
		t.Fatal("IsPath prefix check wrong")
	}
	if Token != "$TXCO_STACK_DIR" {
		t.Fatalf("Token = %q", Token)
	}
}

func TestPackIsCanonical(t *testing.T) {
	a := []File{
		{Path: "2100_SETUP/race.py", Content: []byte("print(1)\n")},
		{Path: "bin/run", Exec: true, Content: []byte("#!/bin/sh\n")},
		{Path: "2100_SETUP/setup.txcl", Content: []byte("WHEN 1 == 1\n")},
	}
	b := []File{a[2], a[0], a[1]}
	da, ha, err := Pack(a)
	if err != nil {
		t.Fatal(err)
	}
	db, hb, err := Pack(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb || !bytes.Equal(da, db) {
		t.Fatal("file order changed the bundle")
	}
	var got []File
	if err := Unpack(da, func(f File) error { got = append(got, f); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Path != "2100_SETUP/race.py" || got[2].Path != "bin/run" {
		t.Fatalf("unpack order = %+v", got)
	}
	if got[0].Exec || !got[2].Exec || string(got[2].Content) != "#!/bin/sh\n" {
		t.Fatalf("modes or content lost: %+v", got)
	}
	// A different executable bit is a different tree.
	a[1].Exec = false
	if _, hc, _ := Pack(a); hc == ha {
		t.Fatal("the executable bit is not part of the bundle")
	}
}

func TestLongPathsRoundTrip(t *testing.T) {
	long := strings.Repeat("deep/", 60) + "file.txt" // past ustar's 255-byte limit
	data, _, err := Pack([]File{{Path: long, Content: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := Unpack(data, func(f File) error { got = f.Path; return nil }); err != nil || got != long {
		t.Fatalf("long path round trip: %q, %v", got, err)
	}
}

func TestPackRefuses(t *testing.T) {
	big := bytes.Repeat([]byte("x"), MaxFileBytes+1)
	cases := map[string][]File{
		"absolute":       {{Path: "/etc/passwd"}},
		"dotdot":         {{Path: "a/../b"}},
		"dot segment":    {{Path: "a/.hidden"}},
		"unclean":        {{Path: "a//b"}},
		"empty":          {{Path: ""}},
		"twice":          {{Path: "a"}, {Path: "a"}},
		"file as dir":    {{Path: "a"}, {Path: "a.x"}, {Path: "a/b"}},
		"file too large": {{Path: "a", Content: big}},
	}
	for name, fs := range cases {
		if _, _, err := Pack(fs); err == nil {
			t.Errorf("%s: Pack accepted %v", name, fs)
		}
	}
	// The whole tree is capped too.
	var many []File
	chunk := bytes.Repeat([]byte("y"), MaxFileBytes)
	for i := 0; i <= MaxTreeBytes/MaxFileBytes; i++ {
		many = append(many, File{Path: fmt.Sprintf("f%03d", i), Content: chunk})
	}
	if _, _, err := Pack(many); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("a tree over the cap packed: %v", err)
	}
}

func TestUnpackRefusesForeignEntries(t *testing.T) {
	foreign := func(hdr *tar.Header) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		return buf.Bytes()
	}
	for name, data := range map[string][]byte{
		"directory": foreign(&tar.Header{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755}),
		"symlink":   foreign(&tar.Header{Typeflag: tar.TypeSymlink, Name: "a", Linkname: "/etc", Mode: 0o777}),
		"escape":    foreign(&tar.Header{Typeflag: tar.TypeReg, Name: "../x", Mode: 0o444}),
		"garbage":   []byte(strings.Repeat("not a tar ", 100)),
	} {
		if err := Unpack(data, func(File) error { return nil }); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
	good, _, _ := Pack([]File{{Path: "ok", Content: []byte("1")}})
	if err := Unpack(good, func(File) error { return nil }); err != nil {
		t.Fatalf("a good bundle refused: %v", err)
	}
}
