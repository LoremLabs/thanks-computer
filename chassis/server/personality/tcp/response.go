package tcp

import (
	"encoding/base64"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// getOutput convert a body from base64, or return json
func getOutput(output string, hidePrivate bool) ([]byte, error) {

	b64BodyString := gjson.Get(output, "_txc.server.write").String()
	if b64BodyString == "" {
		// no body = return raw output

		// Per-event override: if _txc.flag_private is true, keep
		// underscore-prefixed fields even when chassis config would
		// strip them. Mirrors the web inlet's behavior.
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
      if (output == "{}") {
        output = ""
      }
      return []byte(output), nil // without extra newline
		}

    return []byte(output + "\n"), nil // with extra newline
	}

	decoded, err := base64.StdEncoding.DecodeString(b64BodyString)
	if err != nil {
		return nil, err
	}
	return decoded, nil // no extra \n
}
