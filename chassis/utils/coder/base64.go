package coder

import (
	"encoding/base64"
)

func Base64Encode(str string)  string {
	input := []byte(str)
	encodeString := base64.StdEncoding.EncodeToString(input)
	return encodeString
}

func Base64Decode(encodeString string) string {
	decodeBytes, err := base64.StdEncoding.DecodeString(encodeString)
	if err != nil {
		return ""
	}
	return string(decodeBytes)
}
