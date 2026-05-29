package funcs

// `try_*` variants of the strict functions in builtin.go. Each
// invokes the corresponding strict form and substitutes nil on
// error instead of propagating the error up to a rule halt.
//
// This is the safety valve for callers who genuinely want to
// recover from a malformed input (third-party webhooks with
// inconsistent payloads, optional fields, etc.) — visible at the
// call site so a reader knows immediately whether failure halts
// or continues.
//
// Pure functions that can't really fail (&uuid, &now, &concat,
// &len, &sha256, &object, &array, &b64encode, &urlencode,
// &to_json, &has, &split) don't get a try_ variant. Adding one
// would be a footgun — it would suggest there's a recoverable
// failure mode when there isn't.
//
// Convention: a try_ variant's signature mirrors the strict form
// exactly. On a strict-form error, the try_ wrapper returns
// (nil, nil) — the value flows through the runtime as a normal
// null, and the rule continues. The companion strict form's
// error is intentionally NOT preserved; if a caller wants to
// distinguish "absent" from "failed," they need the strict form
// or a paired `&has`.

func init() {
	register("try_json", tryJSONFn)
	register("try_b64decode", tryB64decodeFn)
	register("try_urldecode", tryURLdecodeFn)
	register("try_get", tryGetFn)
	register("try_substr", trySubstrFn)
}

func tryJSONFn(args []any) (any, error) {
	v, err := jsonFn(args)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

func tryB64decodeFn(args []any) (any, error) {
	v, err := b64decodeFn(args)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

func tryURLdecodeFn(args []any) (any, error) {
	v, err := urldecodeFn(args)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

// tryGetFn wraps getFn — note that getFn already returns nil on a
// missing path (absence is not an error). The try_ form
// additionally swallows the unwalkable-obj case so neither failure
// halts the rule.
func tryGetFn(args []any) (any, error) {
	v, err := getFn(args)
	if err != nil {
		return nil, nil
	}
	return v, nil
}

func trySubstrFn(args []any) (any, error) {
	v, err := substrFn(args)
	if err != nil {
		return nil, nil
	}
	return v, nil
}
