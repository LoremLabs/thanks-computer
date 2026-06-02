package admission

import (
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ShapeDeny rewrites resp into a terminal deny envelope the inlet can
// render. For a web request it sets _txc.web.res.status/headers/body —
// mirroring how stack-emitted responses look (see
// chassis/server/personality/web/response.go) — and always stamps
// _txc.admission.denied so the denial is marked even on non-web sources
// that have no response projection. When tenant is non-empty it is
// stamped onto _txc.tenant so the usage convergence point attributes the
// denial to the right tenant.
//
// The web body is intentionally minimal ("<code> <reason-text>"); no
// internal envelope state leaks, because the inlet writes the explicit
// _txc.web.res.body and strips _-prefixed keys from any JSON projection.
func ShapeDeny(resp string, d Decision, tenant string) string {
	if resp == "" {
		resp = "{}"
	}
	status := d.Status
	if status == 0 {
		status = defaultDenyStatus
	}
	resp, _ = sjson.Set(resp, "_txc.admission.denied", true)
	if tenant != "" {
		resp, _ = sjson.Set(resp, "_txc.tenant", tenant)
	}
	// Response shaping is web-shaped; only emit it for requests that have
	// an HTTP response writer (those carry _txc.web.req). Non-web sources
	// (tcp/cron) get the marker above and a terminated run — full deny
	// shaping for them is future work.
	if !gjson.Get(resp, "_txc.web.req").Exists() {
		return resp
	}
	resp, _ = sjson.Set(resp, "_txc.web.res.status", status)
	resp, _ = sjson.Set(resp, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
	if d.Reason != "" {
		resp, _ = sjson.Set(resp, "_txc.web.res.headers.x-txc-deny-reason.0", d.Reason)
	}
	body := fmt.Sprintf("%d %s\n", status, http.StatusText(status))
	resp, _ = sjson.Set(resp, "_txc.web.res.body", base64.StdEncoding.EncodeToString([]byte(body)))
	return resp
}
