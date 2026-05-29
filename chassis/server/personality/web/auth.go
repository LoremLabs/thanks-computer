package web

import (
	"crypto/subtle"
	"net/http"

	"go.uber.org/zap"
)

// BasicAuth wraps a handler requiring HTTP basic auth for it using the given
// username and password and the specified realm, which shouldn't contain quotes.
//
// Most web browser display a dialog with something like:
//
//    The website says: "<realm>"
//
// Which is really stupid so you may want to set the realm to a message rather than
// an actual realm.
func (web *WebController) BasicAuth(handler http.HandlerFunc, username, password, realm string) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {

		if len(username) != 0 {
			user, pass, ok := r.BasicAuth()

			if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(username)) != 1 || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
				w.WriteHeader(401)
				_, err := w.Write([]byte("Unauthorized\n"))
				if err != nil {
					web.pu.Logger.Info("web auth write error", zap.String("err", err.Error()))
				}
				return
			}
		}

		handler(w, r)
	}
}
