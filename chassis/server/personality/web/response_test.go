package web

import (
	"strconv"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestCheckStatus(t *testing.T) {

	tests := []struct {
    input      string
		output      string
		status int
	}{
		{
			`{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
      `{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
			200,
		},
    {
			`{"_txc":{"web":{"res":{}}}}`,
      `{"_txc":{"web":{"res":{"status":200}}}}`,
			200,
		},
    {
      `{"_txc":{"web":{"res":{"status":"400"}}}}`,
      `{"_txc":{"web":{"res":{"status":400}}}}`,
			400,
		},
    {
      `{"_txc":{"web":{"res":{"status":600}}}}`,
      `{"_txc":{"web":{"res":{"status":200}}}}`,
			200,
		},
    {
      `{"_txc":{"web":{"res":{"status":99}}}}`,
      `{"_txc":{"web":{"res":{"status":200}}}}`,
			200,
		},
	}

	for _, tt := range tests {
		testOut, testStatus := checkStatus(tt.input)
    test.Equals(t, tt.output, testOut)
		test.Equals(t, tt.status, testStatus)
	}
}

func TestCheckContentType(t *testing.T) {

	tests := []struct {
    input      string
		output      string
	}{
		{
			`{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
      `{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
		},
    {
			``,
      `{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]}}}}}`,
		},
    {
      `{"_txc":{"web":{"res":{"headers":{"content-type":["text/plain"]}}}}}`,
      `{"_txc":{"web":{"res":{"headers":{"content-type":["text/plain"]}}}}}`,
		},
	}

	for _, tt := range tests {
		testOut := checkContentType(tt.input)
    test.Equals(t, tt.output, testOut)
	}
}

func TestGetOutput(t *testing.T) {

	tests := []struct {
    input      string
    hide       bool
		output     []byte
	}{
		{
			`{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
      false,
      []byte(`{"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`),
		},
    {
			`{"a":1,"_txc":{"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
      true,
      []byte(`{"a":1}`),
		},
    {
			`{"a":1,"_txc":{"web":{"res":{"body":"YW55ICsgb2xkICYgZGF0YQ==","status":200}}}}`,
      true,
      []byte(`any + old & data`),
		},
    {
			`{"a":1,"_txc":{"web":{"res":{"body":"YW55ICsgb2xkICYgZGF0YQ==","status":200}}}}`,
      false,
      []byte(`any + old & data`),
		},
    {
			`{"a":1,"_txc":{"web":{"res":{"body":"NonSense___YW55ICsgb2xkICYgZGF0YQ==","status":200}}}}`,
      true,
      nil,
		},
		// _txc.flag_private overrides hidePrivate=true: _* fields stay.
		{
			`{"a":1,"_txc":{"flag_private":true,"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`,
			true,
			[]byte(`{"a":1,"_txc":{"flag_private":true,"web":{"res":{"headers":{"content-type":["application/json"]},"status":200}}}}`),
		},
		// flag_private=false (or absent) and hidePrivate=true → strip.
		{
			`{"a":1,"_txc":{"flag_private":false,"info":"x"}}`,
			true,
			[]byte(`{"a":1}`),
		},
	}

	for _, tt := range tests {
		testOut, _ := getOutput(tt.input, tt.hide)
    test.Equals(t, tt.output, testOut)
	}
}

// A failed run (an ErrorStr payload) renders as a failure: 500 unless it
// carries its own status, not cached, and the error alone as the body,
// even with private vars shown (dev).
func TestFailureResponse(t *testing.T) {
	tests := []struct {
		name, input, status, cache, body string
	}{
		{
			"a compute out of wall-clock",
			`{"error":{"message":"compute: wall-clock limit exceeded after 250ms"}}`,
			"500", "no-store", `{"error":{"message":"compute: wall-clock limit exceeded after 250ms"}}`,
		},
		{
			"an abort keeps its 503, and loses its envelope",
			`{"err":"aborted","error":{"code":"txco_run_aborted","message":"aborted"},"_txc":{"web":{"res":{"status":503}}}}`,
			"503", "no-store", `{"err":"aborted","error":{"code":"txco_run_aborted","message":"aborted"}}`,
		},
		{
			"a cancel",
			`{"err":"canceled"}`,
			"500", "no-store", `{"err":"canceled"}`,
		},
		{
			"its own cache-control and body stand",
			`{"error":{"message":"x"},"_txc":{"web":{"res":{"headers":{"cache-control":["private"]},"body":"b29wcw=="}}}}`,
			"500", "private", `oops`,
		},
	}
	for _, tt := range tests {
		out := failureResponse(tt.input)
		out, status := checkStatus(out)
		out = checkContentType(out)
		test.Equals(t, tt.status, strconv.Itoa(status))
		test.Equals(t, tt.cache, gjson.Get(out, "_txc.web.res.headers.cache-control.0").String())
		test.Equals(t, "application/json", gjson.Get(out, "_txc.web.res.headers.content-type.0").String())
		// hidePrivate=false: dev, SHOW_PRIVATE_VARS. The body is still the error alone.
		body, err := getOutput(out, false)
		test.Ok(t, err)
		test.Equals(t, tt.body, string(body))
	}
}
