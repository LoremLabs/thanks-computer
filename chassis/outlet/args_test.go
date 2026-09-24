package outlet

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertArgs(t *testing.T) {
	args, err := ConvertArgs(gjson.Parse(`[null, true, "x", 1, 1.5, 2.0, 1e3, 9007199254740993]`))
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != nil || args[1] != true || args[2] != "x" {
		t.Fatalf("scalars: %#v", args)
	}
	if v, ok := args[3].(int64); !ok || v != 1 {
		t.Fatalf("integral must bind as int64: %#v", args[3])
	}
	if v, ok := args[4].(float64); !ok || v != 1.5 {
		t.Fatalf("fraction must bind as float64: %#v", args[4])
	}
	if _, ok := args[5].(float64); !ok {
		t.Fatalf("2.0 keeps the author's float: %#v", args[5])
	}
	if _, ok := args[6].(float64); !ok {
		t.Fatalf("1e3 keeps the author's float: %#v", args[6])
	}
	if v, ok := args[7].(int64); !ok || v != 9007199254740993 {
		t.Fatalf("large integers stay exact: %#v", args[7])
	}

	args, err = ConvertArgs(gjson.Parse(`[[1, null, 3], ["a", null], [true, false], [1, 2.5], [], [null, null]]`))
	if err != nil {
		t.Fatal(err)
	}
	ints := args[0].([]*int64)
	if len(ints) != 3 || *ints[0] != 1 || ints[1] != nil || *ints[2] != 3 {
		t.Fatalf("int array with null: %#v", ints)
	}
	strs := args[1].([]*string)
	if len(strs) != 2 || *strs[0] != "a" || strs[1] != nil {
		t.Fatalf("string array with null: %#v", strs)
	}
	if b := args[2].([]*bool); !*b[0] || *b[1] {
		t.Fatalf("bool array: %#v", b)
	}
	if f := args[3].([]*float64); *f[0] != 1 || *f[1] != 2.5 {
		t.Fatalf("mixed integral/fraction binds as float: %#v", f)
	}
	if e := args[4].([]*string); len(e) != 0 {
		t.Fatalf("empty array: %#v", e)
	}
	if n := args[5].([]*string); len(n) != 2 || n[0] != nil {
		t.Fatalf("all-null array: %#v", n)
	}

	for _, bad := range []string{`[[1, "two"]]`, `[[1, [2]]]`, `[{"a":1}]`, `"x"`, `[[true, 1]]`} {
		if _, err := ConvertArgs(gjson.Parse(bad)); err == nil || err.Code != CodeInvalidRequest {
			t.Errorf("%s: want invalid_request, got %v", bad, err)
		}
	}
	if args, err := ConvertArgs(gjson.Parse(`{"other": 1}`).Get("args")); err != nil || args != nil {
		t.Fatalf("absent args: %v %v", args, err)
	}
}
