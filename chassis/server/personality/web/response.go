package web

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyResponseHead applies the status and headers from a response
// envelope to w, returning the resolved status. It mirrors the inline
// head-application in the buffered handler (checkStatus + checkContentType
// + the _txc.web.res.headers fan-out) and is used by the streaming path,
// which must commit status + headers before the first body chunk. It does
// NOT call WriteHeader — the caller does, after this returns.
func applyResponseHead(w http.ResponseWriter, output string) int {
	output, status := checkStatus(output)
	output = checkContentType(output)
	gjson.Get(output, "_txc.web.res.headers").ForEach(func(key, value gjson.Result) bool {
		hp := "_txc.web.res.headers." + key.String()
		gjson.Get(output, hp).ForEach(func(k, v gjson.Result) bool {
			w.Header().Set(key.String(), v.String())
			return true
		})
		return true
	})
	return status
}

// getOutput convert a body from base64, or return json
func getOutput(output string, hidePrivate bool) ([]byte, error) {

	b64BodyString := gjson.Get(output, "_txc.web.res.body").String()
	if b64BodyString == "" {
		// no body = return raw output

		// Per-event override: if _txc.flag_private is true, keep
		// underscore-prefixed fields even when chassis config would
		// strip them. Lets a rule (or a chassis stamping it in dev/
		// debug mode) ask for the full envelope without changing
		// chassis-wide config.
		flagPrivate := gjson.Get(output, "_txc.flag_private").Bool()

		// but first check if we should strip out private vars
		if hidePrivate && !flagPrivate {
			// hide vars unless we're told to show them by the config
			gjson.Parse(output).ForEach(func(key, value gjson.Result) bool {
				if strings.HasPrefix(key.String(), "_") {
					output, _ = sjson.Delete(output, key.String())
				}
				return true
			})
		}

		return []byte(output), nil
	}

	decoded, err := base64.StdEncoding.DecodeString(b64BodyString)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

// checkStatus Make sure the response object has a valid status set (100-599)
func checkStatus(output string) (string, int) {
	var status int
	st, err := strconv.ParseInt(gjson.Get(output, "_txc.web.res.status").String(), 10, 64)
	if (err != nil) || (st < 100) || (st > 599) {
		st = 200
	}
	status = int(st)
	output, _ = sjson.Set(output, "_txc.web.res.status", status)

	return output, status
}

// checkContentType Make sure the response object has a valid content type, defaulting if needed
func checkContentType(output string) string {
	// add a default content-type if we don't have one already
	ct := gjson.Get(output, "_txc.web.res.headers.content-type.0").String()
	if ct == "" {
		ct = "application/json"
	}
	output, _ = sjson.Set(output, "_txc.web.res.headers.content-type.0", ct)
	return output
}
