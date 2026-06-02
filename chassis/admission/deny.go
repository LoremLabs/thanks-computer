package admission

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// MarkDenied stamps the transport-neutral admission-denial marker onto
// resp: _txc.admission.{denied,status,reason}, plus _txc.tenant (when
// non-empty) so the usage convergence point attributes the denial to the
// right tenant. It is deliberately transport-agnostic — each personality's
// outlet reads this marker (via Denied) and renders the rejection in its
// own protocol: web → HTTP status, lmtp → SMTP code, tcp → line+close,
// cron → log. The shared gate must not know about transports.
func MarkDenied(resp string, d Decision, tenant string) string {
	if resp == "" {
		resp = "{}"
	}
	status := d.Status
	if status == 0 {
		status = defaultDenyStatus
	}
	resp, _ = sjson.Set(resp, "_txc.admission.denied", true)
	resp, _ = sjson.Set(resp, "_txc.admission.status", status)
	if d.Reason != "" {
		resp, _ = sjson.Set(resp, "_txc.admission.reason", d.Reason)
	}
	if tenant != "" {
		resp, _ = sjson.Set(resp, "_txc.tenant", tenant)
	}
	return resp
}

// Denied reports whether resp carries an admission-denial marker, and if
// so the status + reason an outlet should render. Each personality calls
// this in its response path to map the neutral denial to its protocol.
func Denied(resp string) (status int, reason string, ok bool) {
	if !gjson.Get(resp, "_txc.admission.denied").Bool() {
		return 0, "", false
	}
	status = int(gjson.Get(resp, "_txc.admission.status").Int())
	if status == 0 {
		status = defaultDenyStatus
	}
	return status, gjson.Get(resp, "_txc.admission.reason").String(), true
}
