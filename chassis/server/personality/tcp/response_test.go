package tcp

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestGetOutput(t *testing.T) {

	tests := []struct {
		input  string
		hide   bool
		output []byte
	}{
		{
			`{"_txc":{"client":{"body":"YXNkZg0K","ip":"192.168.0.38"},"rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}`,
			false,
			[]byte(`{"_txc":{"client":{"body":"YXNkZg0K","ip":"192.168.0.38"},"rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}` + "\n"),
		},
		{
			`{"_txc":{"client":{"body":"YXNkZg0K","ip":"192.168.0.38"},"rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}`,
			true,
			[]byte(``),
		},
		{
			`{"_txc":{"server":{"write":"YW55ICsgb2xkICYgZGF0YQ==","rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}`,
			false,
			[]byte(`any + old & data`),
		},
		{
			`{"_txc":{"server":{"write":"YW55I this isn't a thing {}Csgb2xkICYgZGF0YQ==","rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}`,
			true,
			nil,
		},
		{
			`{"_txc":{"server":{"write":"YW55I this isn't a thing {}Csgb2xkICYgZGF0YQ==","rid":"2nbbyV9QSyLaCHkji","src":"tcp"}}`,
			false,
			nil,
		},
	}

	for _, tt := range tests {
		testOut, _ := getOutput(tt.input, tt.hide)
		// fmt.Printf("\tA:\033[36m%s\033[39m\n\n", tt.output)
		// fmt.Printf("\tB:\033[36m%s\033[39m\n\n", testOut)

		test.Equals(t, tt.output, testOut)
	}
}
