package admin

import "testing"

// TestCleanDemoBuildErr pins the noise-stripping behavior the demo's
// error banner relies on. Esbuild bundle errors get a "bundle <name>:"
// wrapper + a "<path>:LINE:COL: <message>" body; javy errors get a
// "compile error in <path>:" wrapper + similar body. We want the diagnostic
// (LINE:COL: <message>), not the wrapper or the leaking absolute temp path.
func TestCleanDemoBuildErr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"esbuild bundle error with absolute temp path",
			"bundle entry-3461509872.js:\n" +
				"../../../../../private/var/folders/4t/2nmfzj2922sgvl0wr9zj5t0r0000gn/T/txco-demo-compute/entry-3461509872.js:8:5: Expected \";\" but found \"alert\"",
			`8:5: Expected ";" but found "alert"`,
		},
		{
			"javy compile error wrapper stripped",
			"compile error in /tmp/foo/entry-12.js:\n" +
				"/tmp/foo/entry-12.js:3:1: ReferenceError: bogus is not defined",
			`3:1: ReferenceError: bogus is not defined`,
		},
		{
			"javy-not-on-PATH install hint passes through unchanged",
			"javy not found on PATH; install it from github.com/bytecodealliance/javy/releases",
			"javy not found on PATH; install it from github.com/bytecodealliance/javy/releases",
		},
		{
			"a stray line:col-looking pattern in the prose stays inside the message",
			"bundle entry-1.js:\n" +
				"/tmp/foo/entry-1.js:12:7: Expected number 10:20 but got string",
			`12:7: Expected number 10:20 but got string`,
		},
		{
			"empty / unmatched input falls through to raw",
			"some unrelated error",
			"some unrelated error",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanDemoBuildErr(tc.in)
			if got != tc.want {
				t.Errorf("cleanDemoBuildErr mismatch\nin:   %q\nwant: %q\ngot:  %q", tc.in, tc.want, got)
			}
		})
	}
}
