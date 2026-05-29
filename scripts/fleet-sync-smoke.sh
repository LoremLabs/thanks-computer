#!/usr/bin/env bash
# scripts/fleet-sync-smoke.sh
#
# End-to-end operational smoke for the P1 fleet-sync producer +
# consumer pipeline. Cold-boots a single chassis configured as both
# producer (feed-sink=file) AND consumer (feed-source=file) against
# the same shared directory — the self-loop proves every leg of the
# round-trip on a real running binary:
#
#   1. Migrations land: control_events_outbox + applied_events tables
#      exist in runtime.db.
#   2. controlpublish pump starts when feed-sink != nop.
#   3. controlapply applier starts when feed-source != nop.
#   4. handleActivateStack uploads the artifact + writes an outbox row
#      in the activation tx (gated on FeedSink != nop).
#   5. Pump drains the outbox via file Sink → event JSON appears in
#      the feed dir; the row gets published_control_version stamped.
#   6. Applier polls the feed → fetches the artifact → applies → marks
#      applied_events. The chassis's own event flows back through it,
#      idempotently (applied_events guard).
#
# What this does NOT cover: the JetStream-backed Sink/Source (P2,
# lives in the service overlay) and the remaining mutation hooks
# (P5: tenant.created, hostname.*, actor/key/membership.changed).
# Those are exercised by their own integration tests when shipped.
#
# Usage:
#   scripts/fleet-sync-smoke.sh          # build txco + run
#   TXCO=/path/to/txco scripts/fleet-sync-smoke.sh   # use pre-built binary
#
# Exit status: 0 on full pass, 1 on any failed check. Failed runs
# preserve $LOG_DIR + the workspace for inspection; passing runs
# clean everything up.

set -uo pipefail

# --- knobs ---
CHASSIS_PORT=18283
CHASSIS_URL="http://localhost:${CHASSIS_PORT}"
TENANT_SLUG=fleet-smoke
STACK_NAME=hello
# Distinctive content so grep can confirm the artifact made it
# through end-to-end.
STACK_TXCL='EXEC "txco://echo"'

# --- locate the binary ---
TXCO="${TXCO:-}"
BUILT_TXCO=
if [[ -z "${TXCO}" ]]; then
    REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
    if [[ -z "${REPO_ROOT}" ]]; then
        echo "FAIL: not inside a git repo and TXCO not set" >&2
        exit 1
    fi
    BUILT_TXCO="$(mktemp -t txco-fleet-smoke-XXXXXX)"
    echo "==> building txco binary at ${BUILT_TXCO}..."
    ( cd "${REPO_ROOT}" && go build -o "${BUILT_TXCO}" ./cmd/txco ) || {
        echo "FAIL: go build failed" >&2
        rm -f "${BUILT_TXCO}"
        exit 1
    }
    TXCO="${BUILT_TXCO}"
fi
# Resolve to an absolute path — the script `cd`s into the workspace
# before exec'ing TXCO, so a caller-supplied relative path (e.g.
# `TXCO=./chassis/bin/txco`) would break.
if [[ "${TXCO}" != /* ]]; then
    if [[ -f "${TXCO}" ]]; then
        TXCO="$(cd "$(dirname "${TXCO}")" && pwd)/$(basename "${TXCO}")"
    else
        echo "FAIL: TXCO=${TXCO} not found" >&2
        exit 1
    fi
fi

# --- sandbox + cleanup wiring ---
WORKSPACE="$(mktemp -d -t fleet-smoke-ws-XXXXXX)"
LOG_DIR="$(mktemp -d -t fleet-smoke-logs-XXXXXX)"
FEED_DIR="${WORKSPACE}/feed"
ART_DIR="${WORKSPACE}/artifacts"
DB_DIR="${WORKSPACE}/db"
CHASSIS_LOG="${LOG_DIR}/chassis.log"
CHASSIS_PID=
EXIT_CODE=0

mkdir -p "${FEED_DIR}" "${ART_DIR}" "${DB_DIR}"

cleanup() {
    set +e
    [[ -n "${CHASSIS_PID}" ]] && kill -TERM "${CHASSIS_PID}" 2>/dev/null
    sleep 1
    [[ -n "${CHASSIS_PID}" ]] && kill -KILL "${CHASSIS_PID}" 2>/dev/null
    wait 2>/dev/null
    if [[ "${EXIT_CODE}" == 0 ]]; then
        rm -rf "${WORKSPACE}" "${LOG_DIR}"
        [[ -n "${BUILT_TXCO}" ]] && rm -f "${BUILT_TXCO}"
    else
        echo
        echo "workspace preserved at: ${WORKSPACE}"
        echo "logs preserved at:      ${LOG_DIR}"
        [[ -n "${BUILT_TXCO}" ]] && echo "binary preserved at:    ${BUILT_TXCO}"
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

# Curl with basic auth. Quiet by default; surface stderr only on failure.
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

# Runtime DB path. The chassis stamps -dev because TXCO_ENV defaults
# to dev under `txco dev`.
RUNTIME_DB="${DB_DIR}/runtime-dev.db"

# Poll a sqlite assertion until it returns non-empty or times out.
# Mostly used to wait out the pump/applier ticker (FEED_POLL_PERIOD=1).
sqlite_wait() {
    local query="$1"
    local timeout="${2:-10}"
    local out
    for _ in $(seq 1 "${timeout}"); do
        if [[ -f "${RUNTIME_DB}" ]]; then
            out="$(sqlite3 "${RUNTIME_DB}" "${query}" 2>/dev/null)"
            if [[ -n "${out}" ]]; then
                echo "${out}"
                return 0
            fi
        fi
        sleep 1
    done
    return 1
}

echo "workspace: ${WORKSPACE}"
echo "logs:      ${LOG_DIR}"
echo

# ---------------------------------------------------------------
# 1/6  Boot chassis as both producer AND consumer (file backend)
# ---------------------------------------------------------------
echo "==> [1/6] starting chassis on :${CHASSIS_PORT} (file sink + file source, shared dir)"
cd "${WORKSPACE}"

# Use a high web-inlet port so we don't collide with anything else
# the developer might be running on :8080. The admin port is
# CHASSIS_PORT. TCP head is dropped (no :5050 conflict).
WEB_PORT=$(( CHASSIS_PORT - 1 ))

# `txco serve` directly — avoids txco dev's workspace conventions
# (which hardcode :8080 + .txco/dev/ paths).
"${TXCO}" serve \
    --auth-mode=basic --admin-user=admin --admin-pass=secret \
    --web-addr=":${WEB_PORT}" \
    --admin-addr=":${CHASSIS_PORT}" \
    --personalities=cron,web,admin \
    --db-root-dir="${DB_DIR}" \
    --feed-sink=file \
    --feed-source=file \
    --feed-source-file-dir="${FEED_DIR}" \
    --feed-poll-period=1 \
    --artifact-store=file \
    --artifact-store-file-dir="${ART_DIR}" \
    >"${CHASSIS_LOG}" 2>&1 &
CHASSIS_PID=$!

for i in $(seq 1 30); do
    if curl -fsS "${CHASSIS_URL}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS "${CHASSIS_URL}/healthz" >/dev/null || fail "chassis didn't respond on ${CHASSIS_URL} in 30s"
pass "chassis up"

# ---------------------------------------------------------------
# 2/6  Migrations + controllers wired
# ---------------------------------------------------------------
echo "==> [2/6] verifying P1 schema + controllers"
if [[ ! -f "${RUNTIME_DB}" ]]; then
    fail "runtime db not at expected path: ${RUNTIME_DB}"
fi
# Both new tables present?
TABLES="$(sqlite3 "${RUNTIME_DB}" \
    "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('control_events_outbox','applied_events') ORDER BY name;" \
    2>/dev/null)"
if [[ "${TABLES}" != "applied_events
control_events_outbox" ]]; then
    fail "expected both outbox + applied_events tables; got: ${TABLES//$'\n'/,}"
fi
pass "migrations 0009/0010 applied (outbox + applied_events present)"

# Producer pump started?
if ! grep -q "control-event publisher started" "${CHASSIS_LOG}"; then
    fail "publisher pump did not start (expected 'control-event publisher started' in chassis log)"
fi
pass "controlpublish pump started"

# Consumer applier started? (the existing log line)
if ! grep -q "control-event applier started" "${CHASSIS_LOG}"; then
    fail "consumer applier did not start (expected 'control-event applier started' in chassis log)"
fi
pass "controlapply applier started"

# ---------------------------------------------------------------
# 3/6  Create tenant + draft stack + activate (the producer trigger)
# ---------------------------------------------------------------
echo "==> [3/6] tenant + stack create + activate via admin API"
curl_admin POST /v1/tenants "{\"slug\":\"${TENANT_SLUG}\",\"name\":\"Fleet smoke\"}" >/dev/null \
    || fail "tenant create failed"
pass "tenant created"

DRAFT_RESP="$(curl_admin POST "/v1/tenants/${TENANT_SLUG}/stacks/${STACK_NAME}/draft" '{}')" \
    || fail "draft create failed"
VERSION="$(echo "${DRAFT_RESP}" | sed -E 's/.*"version_number":[[:space:]]*([0-9]+).*/\1/')"
[[ -n "${VERSION}" ]] || fail "couldn't parse version_number from: ${DRAFT_RESP}"
pass "draft created at version ${VERSION}"

# Put the single-op stack file. Build the JSON via jq so the txcl
# content's embedded double quotes are properly escaped.
FILES_BODY="$(jq -n \
    --arg path "100/${STACK_NAME}.txcl" \
    --arg content "${STACK_TXCL}" \
    '{files:[{path:$path,content:$content}]}')"
PUT_BODY="$(mktemp)"
PUT_STATUS="$(curl -sS -u admin:secret -o "${PUT_BODY}" -w '%{http_code}' \
    -X PUT "${CHASSIS_URL}/v1/tenants/${TENANT_SLUG}/stacks/${STACK_NAME}/versions/${VERSION}/files" \
    -H 'Content-Type: application/json' -d "${FILES_BODY}")"
if [[ "${PUT_STATUS}" != "200" ]]; then
    fail "put files: HTTP ${PUT_STATUS}: $(cat "${PUT_BODY}")"
fi
rm -f "${PUT_BODY}"
pass "draft files set"

# Activate — this is the mutation hook P1 instruments.
curl_admin POST "/v1/tenants/${TENANT_SLUG}/stacks/${STACK_NAME}/activate" \
    "{\"version_number\":${VERSION}}" >/dev/null \
    || fail "activate failed"
pass "stack activated (producer hook should have fired)"

# ---------------------------------------------------------------
# 4/6  Producer side: outbox row + artifact + feed file
# ---------------------------------------------------------------
echo "==> [4/6] verifying producer side"

# Outbox row exists for stack.activated. Stamped by the admin handler
# in the SAME tx as the activation, so it must be present immediately.
EVENT_ID="$(sqlite_wait \
    "SELECT event_id FROM control_events_outbox \
      WHERE event_type='stack.activated' \
      ORDER BY id DESC LIMIT 1;" \
    5)"
if [[ -z "${EVENT_ID}" ]]; then
    fail "no outbox row for stack.activated within 5s (handler hook didn't fire?)"
fi
pass "outbox row created (event_id=${EVENT_ID})"

# Pump runs on FEED_POLL_PERIOD=1s ticker; wait up to 10s for the
# publish + writeback. Query only returns non-empty once the pump
# has stamped a control_version.
PUBLISHED_CV="$(sqlite_wait \
    "SELECT published_control_version FROM control_events_outbox \
      WHERE event_id='${EVENT_ID}' AND published_control_version IS NOT NULL;" \
    15)"
if [[ -z "${PUBLISHED_CV}" ]]; then
    fail "outbox row never marked published within 15s; check ${CHASSIS_LOG} for pump errors"
fi
pass "outbox row marked published (control_version=${PUBLISHED_CV})"

# Attempt count should still be 0 — no retries needed.
ATTEMPT_COUNT="$(sqlite3 "${RUNTIME_DB}" \
    "SELECT attempt_count FROM control_events_outbox WHERE event_id='${EVENT_ID}';")"
if [[ "${ATTEMPT_COUNT}" != "0" ]]; then
    fail "expected attempt_count=0, got ${ATTEMPT_COUNT} (pump retried — investigate last_error)"
fi
pass "attempt_count=0 (clean publish on first try)"

# Event JSON file landed in the feed dir.
EVENT_FILE="${FEED_DIR}/${EVENT_ID}.json"
if [[ ! -f "${EVENT_FILE}" ]]; then
    fail "event JSON not written to feed dir: ${EVENT_FILE} (dir: $(ls "${FEED_DIR}"))"
fi
pass "event JSON in feed dir: $(basename "${EVENT_FILE}")"

# And the artifact landed under the file artifact store. The
# producer keys by internal tenant_id (not slug), so resolve via DB.
TENANT_ID="$(sqlite3 "${RUNTIME_DB}" \
    "SELECT tenant_id FROM tenants WHERE slug='${TENANT_SLUG}';")"
[[ -n "${TENANT_ID}" ]] || fail "couldn't resolve tenant_id for slug ${TENANT_SLUG}"
EXPECTED_ART="${ART_DIR}/stacks/${TENANT_ID}/${STACK_NAME}/${VERSION}"
if [[ ! -f "${EXPECTED_ART}" ]]; then
    fail "artifact not at expected path: ${EXPECTED_ART}"
fi
# grep for an unambiguous substring that survives JSON-escaping
# (txco:// can't be inside a string-escape sequence).
if ! grep -q 'txco://echo' "${EXPECTED_ART}"; then
    fail "artifact does not contain expected stack content"
fi
pass "artifact uploaded with correct stack content (key: stacks/${TENANT_ID}/${STACK_NAME}/${VERSION})"

# Producer log line:
if ! grep -q "control-event published" "${CHASSIS_LOG}"; then
    fail "'control-event published' log line missing"
fi
pass "publish log line present"

# ---------------------------------------------------------------
# 5/6  Consumer side: applied_events row + apply log
# ---------------------------------------------------------------
echo "==> [5/6] verifying consumer side (self-loop apply)"

APPLIED_CV="$(sqlite_wait \
    "SELECT control_version FROM applied_events WHERE event_id='${EVENT_ID}';" 10)"
if [[ -z "${APPLIED_CV}" ]]; then
    fail "applier never marked event_id=${EVENT_ID} as applied within 10s"
fi
if [[ "${APPLIED_CV}" != "${PUBLISHED_CV}" ]]; then
    fail "applied_events.control_version=${APPLIED_CV} != outbox.published_control_version=${PUBLISHED_CV}"
fi
pass "applied_events row exists at control_version=${APPLIED_CV}"

if ! grep -q "control-event applied: stack.activated" "${CHASSIS_LOG}"; then
    fail "'control-event applied: stack.activated' log line missing"
fi
pass "apply log line present"

# Cursor advanced.
CURSOR="$(sqlite3 "${RUNTIME_DB}" \
    "SELECT val FROM varvals WHERE var='txco-control-version';")"
if [[ "${CURSOR}" != "${PUBLISHED_CV}" ]]; then
    fail "cursor=${CURSOR}, expected ${PUBLISHED_CV}"
fi
pass "control-version cursor advanced to ${CURSOR}"

# ---------------------------------------------------------------
# 6/6  Idempotency: re-applying the same event_id is a no-op
# ---------------------------------------------------------------
echo "==> [6/6] idempotency: replaying the event MUST NOT double-apply"

# Touch the file's mtime so the applier re-considers it (the file
# Source doesn't track mtimes, it just lists; the cursor + applied_events
# guard is what protects against double-apply).
ROWS_BEFORE="$(sqlite3 "${RUNTIME_DB}" "SELECT COUNT(*) FROM applied_events;")"

# Wait one more poll period to let the applier re-tick.
sleep 2

ROWS_AFTER="$(sqlite3 "${RUNTIME_DB}" "SELECT COUNT(*) FROM applied_events;")"
if [[ "${ROWS_AFTER}" != "${ROWS_BEFORE}" ]]; then
    fail "applied_events row count grew on replay: ${ROWS_BEFORE} → ${ROWS_AFTER}"
fi
pass "applied_events count stable across re-poll (idempotency holds)"

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------
echo
echo "🎉 all checks passed"
echo
echo "Verified:"
echo "  • migrations land (control_events_outbox + applied_events)"
echo "  • controlpublish pump and controlapply applier both start"
echo "  • handleActivateStack uploads artifact + writes outbox row in tx"
echo "  • pump drains outbox → file Sink → event JSON in feed dir"
echo "  • applier consumes the file Source → fetches artifact → applies"
echo "  • cursor + applied_events advance in lockstep"
echo "  • idempotent replay (applied_events guard catches re-delivery)"
echo
echo "Not in scope here (covered elsewhere):"
echo "  • JetStream-backed Sink/Source — P2, service overlay"
echo "  • Two-chassis fan-in across a real broker — P4"
echo "  • Auth-row sync (actor/key/membership.changed) — deferred while"
echo "    auth.db runs on shared Postgres (no fleet sync needed)"
