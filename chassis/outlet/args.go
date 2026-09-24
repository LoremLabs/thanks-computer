package outlet

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// ConvertArgs turns the WITH `args` array into positional bind values.
//
// Accepted: null, boolean, string, JSON number, and a homogeneous array of
// those — so `WHERE id = ANY($1)` works, which is the answer to the
// variable-length IN list a literal `sql` can't otherwise express.
// Homogeneous means every non-null element binds as the same type; nulls
// are allowed anywhere, since the database's arrays hold them naturally.
// Numbers are one JSON type: an array of numbers binds as integers when
// every element is integral, else as floats.
//
// An integral number binds as an integer — unlike txco://dataset, which
// binds every number as float64 and so can't satisfy an integer parameter.
// Objects and nested arrays are refused: almost certainly a resolve mistake
// in the op, so fail loudly rather than bind their JSON text.
func ConvertArgs(r gjson.Result) ([]any, *Error) {
	if !r.Exists() || r.Type == gjson.Null {
		return nil, nil
	}
	if !r.IsArray() {
		return nil, NewError(CodeInvalidRequest, "WITH args must be an array (use &array(...))")
	}
	elems := r.Array()
	out := make([]any, 0, len(elems))
	for i, a := range elems {
		v, err := convertScalar(a)
		if err == nil {
			out = append(out, v)
			continue
		}
		if !a.IsArray() {
			return nil, NewError(CodeInvalidRequest, fmt.Sprintf("args[%d] is not a scalar or an array of scalars", i))
		}
		arr, aerr := convertArray(a.Array())
		if aerr != nil {
			return nil, NewError(CodeInvalidRequest, fmt.Sprintf("args[%d]: %v", i, aerr))
		}
		out = append(out, arr)
	}
	return out, nil
}

// errNotScalar marks a JSON value that isn't null/bool/string/number.
var errNotScalar = fmt.Errorf("not a scalar")

func convertScalar(a gjson.Result) (any, error) {
	switch a.Type {
	case gjson.Null:
		return nil, nil
	case gjson.True:
		return true, nil
	case gjson.False:
		return false, nil
	case gjson.String:
		return a.String(), nil
	case gjson.Number:
		return convertNumber(a), nil
	}
	return nil, errNotScalar
}

// convertNumber binds an integral number as int64 and anything else as
// float64. The raw text decides: "1e3" and "2.0" are floats even though
// their value is integral, so an author who wrote a float gets a float.
func convertNumber(a gjson.Result) any {
	if !strings.ContainsAny(a.Raw, ".eE") {
		if n, err := strconv.ParseInt(a.Raw, 10, 64); err == nil {
			return n
		}
	}
	return a.Float()
}

// convertArray builds a typed pointer slice — nil elements are NULLs — from
// a homogeneous JSON array. The pointer slices are what the Postgres driver
// binds as int8[], float8[], text[] and bool[].
func convertArray(elems []gjson.Result) (any, error) {
	kind := gjson.Null
	integral := true
	for i, e := range elems {
		switch e.Type {
		case gjson.Null:
			continue
		case gjson.True, gjson.False:
			if kind != gjson.Null && kind != gjson.True {
				return nil, fmt.Errorf("element %d is a boolean but earlier elements are not", i)
			}
			kind = gjson.True
		case gjson.String:
			if kind != gjson.Null && kind != gjson.String {
				return nil, fmt.Errorf("element %d is a string but earlier elements are not", i)
			}
			kind = gjson.String
		case gjson.Number:
			if kind != gjson.Null && kind != gjson.Number {
				return nil, fmt.Errorf("element %d is a number but earlier elements are not", i)
			}
			kind = gjson.Number
			if _, isInt := convertNumber(e).(int64); !isInt {
				integral = false
			}
		default:
			return nil, fmt.Errorf("element %d is not a scalar (nested arrays and objects are not bindable)", i)
		}
	}
	switch kind {
	case gjson.True:
		out := make([]*bool, len(elems))
		for i, e := range elems {
			if e.Type != gjson.Null {
				b := e.Bool()
				out[i] = &b
			}
		}
		return out, nil
	case gjson.String:
		out := make([]*string, len(elems))
		for i, e := range elems {
			if e.Type != gjson.Null {
				s := e.String()
				out[i] = &s
			}
		}
		return out, nil
	case gjson.Number:
		if integral {
			out := make([]*int64, len(elems))
			for i, e := range elems {
				if e.Type != gjson.Null {
					n := convertNumber(e).(int64)
					out[i] = &n
				}
			}
			return out, nil
		}
		out := make([]*float64, len(elems))
		for i, e := range elems {
			if e.Type != gjson.Null {
				f := e.Float()
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, fmt.Errorf("element %d is not a finite number", i)
				}
				out[i] = &f
			}
		}
		return out, nil
	}
	// Every element null (or an empty array): a NULL-only text[] is the
	// least surprising binding and Postgres will cast it where needed.
	return make([]*string, len(elems)), nil
}
