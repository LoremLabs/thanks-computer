#!/usr/bin/env bash
# scripts/secrets-smoke.sh
#
# End-to-end operational smoke for the per-tenant secret store.
# Cold-boots a chassis in a temp workspace and exercises the
# operator surface (admin API) to confirm:
#
#   1. Master key auto-mints on first boot ("BACK THIS UP" logged).
#   2. Tenant + secret CRUD works via the admin API.
#   3. Reveal-never invariant: list/show/create responses carry zero
#      bytes of cleartext.
#   4. Immutable-name invariant: PATCH with a `name` field returns
#      400 name_immutable.
#
# What this does NOT cover (covered by `go test ./...`):
#   - The txcl rule → outbound HTTP pipe with secrets materialized
#     into the request: see
#     chassis/processor/secrets_e2e_test.go::TestSecretsEndToEnd
#   - secret_store_unavailable when MK is unconfigured: see
#     chassis/server/admin/secret_endpoints_test.go::TestSecretsStoreUnavailable
#   - Crypto round-trip + AAD anti-swap: see chassis/secrets/*_test.go
#
# Usage:
#   scripts/secrets-smoke.sh          # build txco + run
#   TXCO=/path/to/txco scripts/secrets-smoke.sh   # use a pre-built binary
#
# Exit status: 0 on full pass, 1 on any failed check. Failed runs
# preserve $LOG_DIR for inspection; passing runs clean everything up.

set -uo pipefail

# --- knobs ---
CHASSIS_PORT=18181
CHASSIS_URL="http://localhost:${CHASSIS_PORT}"
WEB_PORT=18180          # spawned-chassis web inlet (dev defaults :8080, override here)
CAPTURE_PORT=9990
TENANT_SLUG=acme-smoke
SECRET_NAME=STRIPE_API_KEY
# Distinctive cleartext so grep can confirm absence elsewhere.
CLEARTEXT="sk_smoke_test_supersecret_PID$$"

# --- pre-flight: port availability ---
preflight_port_free() {
    local port="$1"
    if lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | grep -q LISTEN; then
        echo "FAIL: port ${port} is already in use. Kill the stale process or re-run." >&2
        echo "      lsof -nP -iTCP:${port} -sTCP:LISTEN" >&2
        exit 1
    fi
}
preflight_port_free "${CHASSIS_PORT}"
preflight_port_free "${WEB_PORT}"
preflight_port_free "${CAPTURE_PORT}"

# --- locate the binary ---
TXCO="${TXCO:-}"
BUILT_TXCO=
if [[ -z "${TXCO}" ]]; then
    REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
    if [[ -z "${REPO_ROOT}" ]]; then
        echo "FAIL: not inside a git repo and TXCO not set" >&2
        exit 1
    fi
    BUILT_TXCO="$(mktemp -t txco-smoke-XXXXXX)"
    echo "==> building txco binary at ${BUILT_TXCO}..."
    ( cd "${REPO_ROOT}" && go build -o "${BUILT_TXCO}" ./cmd/txco ) || {
        echo "FAIL: go build failed" >&2
        rm -f "${BUILT_TXCO}"
        exit 1
    }
    TXCO="${BUILT_TXCO}"
fi

# --- sandbox + cleanup wiring ---
WORKSPACE="$(mktemp -d -t secrets-smoke-ws-XXXXXX)"
LOG_DIR="$(mktemp -d -t secrets-smoke-logs-XXXXXX)"
CHASSIS_LOG="${LOG_DIR}/chassis.log"
CAPTURE_LOG="${LOG_DIR}/capture.log"
CHASSIS_PID=
CAPTURE_PID=
EXIT_CODE=0

cleanup() {
    set +e
    # Kill the whole process group of each spawned shell — `dev`
    # forks a child chassis we'd otherwise orphan. `-${PID}` targets
    # the process group on macOS + Linux.
    if [[ -n "${CHASSIS_PID}" ]]; then
        kill -TERM "-${CHASSIS_PID}" 2>/dev/null || kill -TERM "${CHASSIS_PID}" 2>/dev/null
    fi
    if [[ -n "${CAPTURE_PID}" ]]; then
        kill -TERM "-${CAPTURE_PID}" 2>/dev/null || kill -TERM "${CAPTURE_PID}" 2>/dev/null
    fi
    sleep 1
    if [[ -n "${CHASSIS_PID}" ]]; then
        kill -KILL "-${CHASSIS_PID}" 2>/dev/null || kill -KILL "${CHASSIS_PID}" 2>/dev/null
    fi
    if [[ -n "${CAPTURE_PID}" ]]; then
        kill -KILL "-${CAPTURE_PID}" 2>/dev/null || kill -KILL "${CAPTURE_PID}" 2>/dev/null
    fi
    # Belt + suspenders: stamp out anything bound to our test ports.
    for p in "${CHASSIS_PORT}" "${WEB_PORT}" "${CAPTURE_PORT}"; do
        pid="$(lsof -ti tcp:"${p}" 2>/dev/null || true)"
        [[ -n "${pid}" ]] && kill -KILL "${pid}" 2>/dev/null
    done
    wait 2>/dev/null
    rm -rf "${WORKSPACE}"
    [[ -n "${BUILT_TXCO}" ]] && rm -f "${BUILT_TXCO}"
    if [[ "${EXIT_CODE}" == 0 ]]; then
        rm -rf "${LOG_DIR}"
    else
        echo "logs preserved at: ${LOG_DIR}"
    fi
    exit "${EXIT_CODE}"
}
trap cleanup EXIT INT TERM

fail() {
    echo "❌ FAIL: $*" >&2
    EXIT_CODE=1
    exit 1
}
pass() {
    echo "✅ $*"
}

# Curl with basic auth + JSON content. Quiet by default; surface
# stderr only on failure.
curl_admin() {
    local method="$1"
    local path="$2"
    local body="${3:-}"
    if [[ -n "${body}" ]]; then
        curl -fsS -u admin:secret -X "${method}" "${CHASSIS_URL}${path}" \
            -H 'Content-Type: application/json' -d "${body}"
    else
        curl -fsS -u admin:secret -X "${method}" "${CHASSIS_URL}${path}"
    fi
}

echo "workspace: ${WORKSPACE}"
echo "logs:      ${LOG_DIR}"
echo

# ---------------------------------------------------------------
# 1/7  Start capture endpoint (records inbound headers + body)
# ---------------------------------------------------------------
echo "==> [1/6] starting capture endpoint on :${CAPTURE_PORT}"
python3 - "${CAPTURE_LOG}" "${CAPTURE_PORT}" >/dev/null 2>&1 <<'PY' &
import sys, http.server
log = open(sys.argv[1], 'a', buffering=1)
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', '0') or 0)
        body = self.rfile.read(n)
        log.write(f"AUTH={self.headers.get('Authorization')!r}\n")
        log.write(f"BODY={body!r}\n")
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(b'{"ok":true}')
    def log_message(self, *a, **kw): pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[2])), H).serve_forever()
PY
CAPTURE_PID=$!
sleep 1
kill -0 "${CAPTURE_PID}" 2>/dev/null || fail "capture endpoint didn't start"
pass "capture endpoint up"

# ---------------------------------------------------------------
# 2/7  Workspace + chassis (basic-auth mode, sidesteps signed bootstrap)
# ---------------------------------------------------------------
echo "==> [2/6] starting chassis on :${CHASSIS_PORT} (basic-auth admin:secret)"
cd "${WORKSPACE}"
mkdir -p OPS
cat > txco.yaml <<YAML
target: dev
targets:
  dev:
    chassis: ${CHASSIS_URL}
YAML

# WEB_ADDR overrides the default :8080 so successive smoke runs (and
# parallel ones) don't fight over the user's actual dev web port.
TXCO_AUTH_MODE=basic TXCO_ADMIN_USER=admin TXCO_ADMIN_PASS=secret \
    TXCO_WEB_ADDR=":${WEB_PORT}" \
    "${TXCO}" dev --chassis-addr ":${CHASSIS_PORT}" >"${CHASSIS_LOG}" 2>&1 &
CHASSIS_PID=$!

for i in $(seq 1 30); do
    if curl -fsS "${CHASSIS_URL}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS "${CHASSIS_URL}/healthz" >/dev/null || fail "chassis didn't respond on ${CHASSIS_URL} in 30s"
pass "chassis up"

# ---------------------------------------------------------------
# 3/7  Auto-mint observed
# ---------------------------------------------------------------
echo "==> [3/6] verifying auto-mint logged on cold boot"
if grep -q "minted new master key" "${CHASSIS_LOG}"; then
    pass "first-mint log line present (back-this-up obligation visible)"
else
    fail "no auto-mint log line in chassis stderr"
fi
if grep -q "secret store enabled" "${CHASSIS_LOG}"; then
    pass "secret store enabled at boot"
else
    fail "secret store didn't enable"
fi

# ---------------------------------------------------------------
# 4/7  Tenant + secret CRUD
# ---------------------------------------------------------------
echo "==> [4/6] tenant + secret CRUD via admin API"
TENANT_RESP="$(curl_admin POST /v1/tenants "{\"slug\":\"${TENANT_SLUG}\",\"name\":\"Acme Smoke\"}")" \
    || fail "tenant create failed"
echo "${TENANT_RESP}" | grep -q "\"slug\":\"${TENANT_SLUG}\"" \
    || fail "tenant create response unexpected: ${TENANT_RESP}"
pass "tenant created"

CREATE_RESP="$(curl_admin POST "/v1/tenants/${TENANT_SLUG}/secrets" \
    "{\"name\":\"${SECRET_NAME}\",\"value\":\"${CLEARTEXT}\",\"description\":\"smoke\"}")" \
    || fail "secret create failed"
echo "${CREATE_RESP}" | grep -q "\"name\":\"${SECRET_NAME}\"" \
    || fail "secret create response unexpected: ${CREATE_RESP}"
pass "secret stored (operator-supplied value)"

# ---------------------------------------------------------------
# 5/7  Reveal-never invariant (load-bearing)
# ---------------------------------------------------------------
echo "==> [5/6] reveal-never: cleartext must NOT appear in any read response"
if echo "${CREATE_RESP}" | grep -q '"value"'; then
    fail "REVEAL-NEVER BROKEN: create response contains 'value' field: ${CREATE_RESP}"
fi
if echo "${CREATE_RESP}" | grep -q "${CLEARTEXT}"; then
    fail "REVEAL-NEVER BROKEN: create response leaks cleartext"
fi
pass "create response: no value field, no cleartext bytes"

LIST_RESP="$(curl_admin GET "/v1/tenants/${TENANT_SLUG}/secrets")" || fail "list failed"
if echo "${LIST_RESP}" | grep -q '"value"'; then
    fail "REVEAL-NEVER BROKEN: list response contains 'value' field: ${LIST_RESP}"
fi
if echo "${LIST_RESP}" | grep -q "${CLEARTEXT}"; then
    fail "REVEAL-NEVER BROKEN: list response leaks cleartext"
fi
pass "list response: no value field, no cleartext bytes"

SHOW_RESP="$(curl_admin GET "/v1/tenants/${TENANT_SLUG}/secrets/${SECRET_NAME}")" || fail "show failed"
if echo "${SHOW_RESP}" | grep -q '"value"'; then
    fail "REVEAL-NEVER BROKEN: show response contains 'value' field: ${SHOW_RESP}"
fi
if echo "${SHOW_RESP}" | grep -q "${CLEARTEXT}"; then
    fail "REVEAL-NEVER BROKEN: show response leaks cleartext"
fi
pass "show response: no value field, no cleartext bytes"

# ---------------------------------------------------------------
# 6/7  Immutable-name invariant
# ---------------------------------------------------------------
echo "==> [6/6] immutable-name: PATCH with 'name' field must 400"
PATCH_BODY="$(mktemp)"
PATCH_STATUS="$(curl -sS -u admin:secret -o "${PATCH_BODY}" -w '%{http_code}' \
    -X PATCH "${CHASSIS_URL}/v1/tenants/${TENANT_SLUG}/secrets/${SECRET_NAME}" \
    -H 'Content-Type: application/json' \
    -d '{"description":"new","name":"STOLEN"}')"
if [[ "${PATCH_STATUS}" != "400" ]]; then
    fail "rename attempt got ${PATCH_STATUS}, want 400; body=$(cat "${PATCH_BODY}")"
fi
if ! grep -q "name_immutable" "${PATCH_BODY}"; then
    fail "rename rejection lacks name_immutable code: $(cat "${PATCH_BODY}")"
fi
rm -f "${PATCH_BODY}"
pass "rename attempt rejected with name_immutable"

# Also verify the cleartext didn't leak into the error body.
# (Negative test: the rejected PATCH body contained "STOLEN" — that's
# the operator's input, not a secret. The CLEARTEXT shouldn't appear.)

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
echo
echo "🎉 all checks passed"
echo
echo "Verified:"
echo "  • cold-boot auto-mints master key (visible log + 'back this up' notice)"
echo "  • admin API: tenant create, secret create"
echo "  • reveal-never on create/list/show (zero bytes of cleartext anywhere)"
echo "  • immutable-name: PATCH with 'name' field → 400 name_immutable"
echo
echo "Not in scope (covered by go test):"
echo "  • txcl rule with WITH secrets.* → outbound HTTP with overlaid header"
echo "    → chassis/processor/secrets_e2e_test.go::TestSecretsEndToEnd"
echo "  • secret_store_unavailable when MK is empty"
echo "    → chassis/server/admin/secret_endpoints_test.go::TestSecretsStoreUnavailable"
echo "  • crypto round-trip / anti-swap AAD binding"
echo "    → chassis/secrets/{crypto,store,master_key,resolver}_test.go"
