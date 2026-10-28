package grantgw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// networkAskTimeout bounds one open over the network, as the socket's does.
const networkAskTimeout = 30 * time.Second

// HandleSandbox serves POST /v1/sandbox/{name} on the web head: a program
// on another machine — a command in a remote workspace, holding the run
// grant's token it was started with — opens one entry of a sandbox the
// grant names. The token is in grantwire.Header, the body is
// {"entry": …}, and the answer is a grantwire.Response, decided exactly as
// the socket's (`@grant.via` "network"): the program learns what it holds,
// that it was refused, or that the chassis could not decide.
func (g *Gateway) HandleSandbox(w http.ResponseWriter, r *http.Request) {
	answer := func(status int, resp grantwire.Response) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}
	token := strings.TrimSpace(r.Header.Get(grantwire.Header))
	name := mux.Vars(r)["name"]
	if token == "" {
		answer(http.StatusUnauthorized, grantwire.Response{Error: grantwire.ErrorRefused})
		return
	}
	var body struct {
		Entry string `json:"entry"`
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, grantwire.MaxRequest+1))
	if err != nil || len(data) > grantwire.MaxRequest {
		answer(http.StatusBadRequest, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	if len(strings.TrimSpace(string(data))) > 0 && json.Unmarshal(data, &body) != nil {
		answer(http.StatusBadRequest, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	if !sandbox.ValidName(name) {
		answer(http.StatusBadRequest, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	if _, _, ok := sandbox.ParseEntry(body.Entry); body.Entry != "" && !ok {
		answer(http.StatusBadRequest, grantwire.Response{Error: grantwire.ErrorBadRequest})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), networkAskTimeout)
	defer cancel()
	ans := g.OpenSandbox(ctx, Open{Token: token, Sandbox: name, Via: ViaNetwork, Entry: body.Entry})
	switch {
	case ans.Allowed:
		env := make(map[string]string, len(ans.Env))
		for v, value := range ans.Env {
			env[v] = base64.StdEncoding.EncodeToString(value)
		}
		answer(http.StatusOK, grantwire.Response{OK: true, Env: env, Serve: ans.Serve})
		ans.Zero()
	case ans.Unavailable:
		answer(http.StatusServiceUnavailable, grantwire.Response{Error: grantwire.ErrorUnavailable})
	default:
		// One answer for every refusal: the program is not told why.
		answer(http.StatusForbidden, grantwire.Response{Error: grantwire.ErrorRefused})
	}
}
