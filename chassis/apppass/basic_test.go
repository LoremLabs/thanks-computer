package apppass

import (
	"encoding/base64"
	"testing"
)

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestBasicHeaderMatches(t *testing.T) {
	pw := []byte("correct horse battery staple")
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"match", basic("print", string(pw)), true},
		{"scheme is case-insensitive", "bAsIc " + base64.StdEncoding.EncodeToString([]byte("print:"+string(pw))), true},
		{"password may contain colons", basic("print", "a:b:c"), false},
		{"wrong password", basic("print", "nope"), false},
		{"wrong user", basic("other", string(pw)), false},
		{"both wrong", basic("other", "nope"), false},
		{"empty password never matches a set one", basic("print", ""), false},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("print")), false},
		{"not base64", "Basic !!!", false},
		{"other scheme", "Bearer abc", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := BasicHeaderMatches(c.header, "print", pw); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// The user-id ends at the FIRST colon: a password containing colons works.
	if !BasicHeaderMatches(basic("print", "a:b:c"), "print", []byte("a:b:c")) {
		t.Error("password containing colons must match")
	}
}
