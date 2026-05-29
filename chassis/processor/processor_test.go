package processor

import (
	"encoding/json"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

var manyJSON = `{
  "":{"a":"emptya","b":"emptyb"},
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"hello":"world"
  }}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}},
  "age":31,
  "position":{"type":"Point","coordinates":[-115.24,33.09]},
  "loves":["world peace"],
  "name":{"last":"Anderson","first":"Nancy"},
  "name.last":"Yellow",
  "name.first":"Cat"
  }`

var complicatedJSON = `{
"tagged": "OK",
"Tagged": "KO",
"NotTagged": true,
"unsettable": 101,
"Nested": {
  "Yellow": "Green",
  "yellow": "yellow"
},
"nestedTagged": {
  "Green": "Green",
  "Map": {
    "this": "that",
    "and": "the other thing"
  },
  "Ints": {
    "Uint": 99,
    "Uint16": 16,
    "Uint32": 32,
    "Uint64": 65
  },
  "Uints": {
    "int": -99,
    "Int": -98,
    "Int16": -16,
    "Int32": -32,
    "int64": -64,
    "Int64": -65
  },
  "Floats": {
    "Float32": 32.32,
    "Float64": 64.64
  },
  "Byte": 254,
  "Bool": true
},
"LeftOut": "you shouldn't be here",
"SelfPtr": {
  "tagged": "OK",
  "nestedTagged": {
    "Ints": {
      "Uint32": 32
    }
  }
},
"SelfSlice": [{
  "tagged": "OK",
  "nestedTagged": {
    "Ints": {
      "Uint32": 32
    }
  }
}],
"SelfSlicePtr": [{
  "tagged": "OK",
  "nestedTagged": {
    "Ints": {
      "Uint32": 32
    }
  }
}],
"SelfPtrSlice": [{
  "tagged": "OK",
  "nestedTagged": {
    "Ints": {
      "Uint32": 32
    }
  }
}],
"interface": "Tile38 Rocks!",
"Interface": "Please Download",
"Array": [0, 2, 3, 4, 5],
"time": "2017-05-07T13:24:43-07:00",
"Binary": "R0lGODlhPQBEAPeo",
"NonBinary": [9, 3, 100, 115]
}`

func TestMerge(t *testing.T) {

	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
	// "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"hello":"world"
	// }}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}},
	// "":{"a":"emptya","b":"emptyb"},
	// "position":{"type":"Point","coordinates":[-115.24,33.09]},
	// "loves":["world peace"],
	// "name":{"last":"Anderson","first":"Nancy"},
	// "name.last":"Yellow",
	// "name.first":"Cat"

	tests := []struct {
		left   string
		right  string
		merged string
		errStr string
	}{
		{
			`{"num":6.13,"strs":[]}`,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"num":6.13,"strs":["a","b"]}`,
			"",
		},
		{
			`{"moo":{"foo":2},"baz":"moose"}`,
			`{"matched":true,"moo":{"foo":2}}`,
			`{"matched":true,"moo":{"foo":2},"baz":"moose"}`,
			"",
		},
		{
			`{"moo":"1"}`,
			`{}`,
			`{"moo":"1"}`,
			"",
		},
		{
			`{}`,
			`{"moo":"1"}`,
			`{"moo":"1"}`,
			"",
		},
		{
			`{"moo":1}`,
			`{}`,
			`{"moo":1}`,
			"",
		},
		{
			`{"moo":1}`,
			`{"zoo":["a"]}`,
			`{"zoo":["a"],"moo":1}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"]}`,
			`{"zoo":["b"]}`,
			`{"moo":1,"zoo":["a","b"]}`,
			"",
		},
		{
			`{"zoo":["b"]}`,
			`{"moo":1,"zoo":["a"]}`,
			`{"moo":1,"zoo":["b","a"]}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"]}`,
			`{"zoo":["b"],"foo":{}}`,
			`{"foo":{},"moo":1,"zoo":["a","b"]}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"]}`,
			`{"zoo":["b"],"foo":{"thing":true}}`,
			`{"foo":{"thing":true},"moo":1,"zoo":["a","b"]}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"],"foo":{"bar":{"zap":"os"}}}`,
			`{"zoo":["b"],"foo":{"thing":true}}`,
			`{"moo":1,"zoo":["a","b"],"foo":{"bar":{"zap":"os"},"thing":true}}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false], "bar":{"zap":"os"}}}`,
			`{"zoo":["b"],"foo":{"thing":true}}`,
			`{"moo":1,"zoo":["a","b"],"foo":{"bar":{"zap":"os"},"baz":[1,2,false],"thing":true}}`,
			"",
		},
		{
			`{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false],"bar":{"zap":"os"}}}`,
			`{"zoo":["b"],"foo":{"thing":true,"baz":[3]}}`,
			`{"moo":1,"zoo":["a","b"],"foo":{"bar":{"zap":"os"},"baz":[1,2,false,3],"thing":true}}`,
			"",
		},
		{
			`{"outter":{"inner":"second"}}`,
			`{"outter":{"inner":{"inner2":"first"}}}`,
			`{"outter":{"inner":{"inner2":"first"}}}`,
			"",
		},
		{
			`{"outter":{"inner":"second"}}`,
			`{"outter":{"inner":{"inner2":1}}}`,
			`{"outter":{"inner":{"inner2":1}}}`,
			"",
		},
		{
			`{"outter":{"inner":"second"}}`,
			`{"outter":{"inner":{"inner2":-1.11}}}`,
			`{"outter":{"inner":{"inner2":-1.11}}}`,
			"",
		},
		{
			`{"address":"214 harvard street"}`,
			`{"name":{"first":"Janet","last":"Prichard"},"asdf":"hrm","age":47}`,
			`{"age":47,"asdf":"hrm","name":{"first":"Janet","last":"Prichard"},"address":"214 harvard street"}`,
			"",
		},
		{
			`{"utf8":"Example emoji, KO: \ud83d\udd13, \ud83c\udfc3 OK: \u2764\ufe0f "}`,
			`{"name":{"first":"Janet","last":"Prichard"},"asd\nf":"hrm","age":47}`,
			`{"age":47,"asd\nf":"hrm","name":{"first":"Janet","last":"Prichard"},"utf8":"Example emoji, KO: \ud83d\udd13, \ud83c\udfc3 OK: \u2764\ufe0f "}`,
			"",
		},
		{
			`{"noop":{"what is a wren?":"a bird"}}`,
			`{"created":"2014-05-16T08:28:06.989Z"}`,
			`{"created":"2014-05-16T08:28:06.989Z","noop":{"what is a wren?":"a bird"}}`,
			"",
		},
		{
			`{"noop":{"what is a wren?":"a bird"}}`,
			`{"items":[1,2,3,{"tags":[1,2,3],"points":[[1,2],[3,4]]},4,5,6,7]}`,
			`{"items":[1,2,3,{"points":[[1,2],[3,4]],"tags":[1,2,3]},4,5,6,7],"noop":{"what is a wren?":"a bird"}}`,
			"",
		},
		{
			`{"items":"none","noop":{"what is a wren?":"a bird"}}`,
			`{"items":[1,2,3,{"tags":[1,2,3],"points":[[1,2],[3,4]]},4,5,6,7]}`,
			`{"items":[1,2,3,{"points":[[1,2],[3,4]],"tags":[1,2,3]},4,5,6,7],"noop":{"what is a wren?":"a bird"}}`,
			"",
		},
		{
			`{"a":false,"items":[1,2,3,{"tags":[1,2,3],"points":[[1,2],[3,4]]},4,5,6,7]}`,
			`{"items":"none","noop":{"what is a wren?":"a bird"}}`,
			`{"noop":{"what is a wren?":"a bird"},"a":false,"items":"none"}`,
			"",
		},
		{
			`{"a":false,"items":[1,2,3,{"tags":[1,2,3],"points":[[1,2],[3,4]]},4,5,6,7]}`,
			`{"items":true,"noop":{"what is a wren?":"a bird"}}`,
			`{"noop":{"what is a wren?":"a bird"},"a":false,"items":true}`,
			"",
		},
		{
			`{"a":false,"items":[1,2,3,{"tags":[1,2,3],"points":[[1,2],[3,4]]},4,5,6,7]}`,
			`{"items":[true],"noop":{"what is a wren?":"a bird"}}`,
			`{"noop":{"what is a wren?":"a bird"},"a":false,"items":[1,2,3,{"points":[[1,2],[3,4]],"tags":[1,2,3]},4,5,6,7,true]}`,
			"",
		},
		{
			manyJSON,
			"",
			manyJSON,
			"Merging requires both sides to be objects",
		},
		{
			manyJSON,
			`{
        "name.last":"Yellow",
        "name.first":"Dog"
        }`,
			`{
  "":{"a":"emptya","b":"emptyb"},
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"hello":"world"
  }}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}},
  "age":31,
  "position":{"type":"Point","coordinates":[-115.24,33.09]},
  "loves":["world peace"],
  "name":{"last":"Anderson","first":"Nancy"},
  "name.last":"Yellow",
  "name.first":"Dog"
  }`,
			"",
		},
		{
			manyJSON,
			complicatedJSON,
			`{"NonBinary":[9,3,100,115],"Binary":"R0lGODlhPQBEAPeo","time":"2017-05-07T13:24:43-07:00","Array":[0,2,3,4,5],"Interface":"Please Download","interface":"Tile38 Rocks!","SelfPtrSlice":[{"nestedTagged":{"Ints":{"Uint32":32}},"tagged":"OK"}],"SelfSlicePtr":[{"nestedTagged":{"Ints":{"Uint32":32}},"tagged":"OK"}],"SelfSlice":[{"nestedTagged":{"Ints":{"Uint32":32}},"tagged":"OK"}],"SelfPtr":{"nestedTagged":{"Ints":{"Uint32":32}},"tagged":"OK"},"LeftOut":"you shouldn't be here","nestedTagged":{"Bool":true,"Byte":254,"Floats":{"Float32":32.32,"Float64":64.64},"Green":"Green","Ints":{"Uint":99,"Uint16":16,"Uint32":32,"Uint64":65},"Map":{"and":"the other thing","this":"that"},"Uints":{"Int":-98,"Int16":-16,"Int32":-32,"Int64":-65,"int":-99,"int64":-64}},"Nested":{"Yellow":"Green","yellow":"yellow"},"unsettable":101,"NotTagged":true,"Tagged":"KO","tagged":"OK",
  "":{"a":"emptya","b":"emptyb"},
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{
  "a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"a":{"hello":"world"
  }}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}}},
  "age":31,
  "position":{"type":"Point","coordinates":[-115.24,33.09]},
  "loves":["world peace"],
  "name":{"last":"Anderson","first":"Nancy"},
  "name.last":"Yellow",
  "name.first":"Cat"
  }`,
			"",
		},
	}

  pu := &Unit{}

	for _, tt := range tests {

		// choose the best resonator
		merged, err := pu.MergeJSON(tt.left, tt.right)
		if err != nil {
			// fmt.Println("err" + err.Error())
			test.Equals(t, tt.errStr, err.Error())
		} else {
			test.Equals(t, tt.merged, merged)
		}

	}
}

func BenchmarkCustomMergerComplicated(b *testing.B) {

	pu := &Unit{}
	for n := 0; n < b.N; n++ {
		_, _ = pu.MergeJSON(manyJSON, complicatedJSON)
	}

	b.ReportAllocs()
}

func BenchmarkGoMergerComplicated(b *testing.B) {

	for n := 0; n < b.N; n++ {
		simpleMerge(manyJSON, complicatedJSON)
	}

	b.ReportAllocs()
}

func BenchmarkCustomMergerSimple(b *testing.B) {
	pu := &Unit{}

	for n := 0; n < b.N; n++ {
		_, _ = pu.MergeJSON(`{}`, `{"a":true}`)
	}

	b.ReportAllocs()
}

func BenchmarkGoMergerSimple(b *testing.B) {

	for n := 0; n < b.N; n++ {
		_ = simpleMerge(`{}`, `{"a":true}`)
	}

	b.ReportAllocs()
}

func BenchmarkCustomMergerMedium(b *testing.B) {
	pu := &Unit{}

	for n := 0; n < b.N; n++ {
		_, _ = pu.MergeJSON(
			`{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false],"bar":{"zap":"os"}}}`,
			`{"zoo":["b"],"foo":{"thing":true,"baz":[3]}}`,
		)
	}

	b.ReportAllocs()
}

func BenchmarkGoMergerMedium(b *testing.B) {

	for n := 0; n < b.N; n++ {
		simpleMerge(
			`{"moo":1,"zoo":["a"],"foo":{"baz":[1,2,false],"bar":{"zap":"os"}}}`,
			`{"zoo":["b"],"foo":{"thing":true,"baz":[3]}}`,
		)
	}

	b.ReportAllocs()
}

func simpleMerge(i1, i2 string) string {

	var merged map[string]interface{}

	err := json.Unmarshal([]byte(i1), &merged)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal([]byte(i2), &merged)
	if err != nil {
		panic(err)
	}

	outdata, _ := json.Marshal(merged)
	return string(outdata)
}
