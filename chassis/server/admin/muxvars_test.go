package admin

import (
	"net/http"

	"github.com/gorilla/mux"
)

// setURLVarsImpl wraps gorilla/mux's SetURLVars so tests can drive
// handlers that read URL vars directly (handlers below the mux router).
// Kept in its own _test.go file because gorilla/mux's import shouldn't
// land in the larger handler test files.
func setURLVarsImpl(r *http.Request, vars map[string]string) *http.Request {
	return mux.SetURLVars(r, vars)
}
