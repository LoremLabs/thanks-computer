package web

import (
	"testing"

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
