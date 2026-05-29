#!/usr/bin/env bash
# scripts/examples-smoke.sh
#
# End-to-end smoke for the example workspaces under examples/. The
# examples are the project's front door; nothing else exercises them,
# so they rot silently. This harness proves each one still loads and
# (where it has an HTTP surface) still serves.
#
# For every examples/<name>/probe.json:
#
#   1. BASELINE (always, hermetic): `txco apply --dry-run` must succeed.
#      Catches the dominant rot — unparseable txcl, dangling op:// refs,
#      renamed builtins. Needs no chassis, no apps, no network.
#
#   2. CHECKS (when probe.json lists any): cold-boot a `txco dev` for that
#      one example on a private port block, apply any hostname bindings,
#      and fire each HTTP check (status + body substring). A passing
#      check transitively proves cold-boot → apply → server-side
#      validate → activate → route → execute.
#
# Examples run SEQUENTIALLY, so the Node apps several examples hardcode
# on the same ports (4100/9009/4242/4200) never collide. If an example's
# app port is already taken on this machine (e.g. your own `txco dev` is
# running), that example's CHECKS are SKIPPED (its baseline still runs).
#
# Usage:
#   scripts/examples-smoke.sh                     # build txco + run
#   TXCO=/path/to/txco scripts/examples-smoke.sh  # use a pre-built binary
#
# Exit status: 0 if every example passes (skips are not failures), 1 on
# any failure. Failed runs preserve $LOG_DIR for inspection.

set -uo pipefail

# --- private port block (never touch the user's :8080/:8081 dev chassis) ---
ADMIN_PORT=18181
WEB_PORT=18180
ADMIN_URL="http://localhost:${ADMIN_PORT}"
WEB_URL="http://localhost:${WEB_PORT}"

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
if [[ -z "${REPO_ROOT}" ]]; then
    echo "FAIL: not inside a git repo and unable to locate the repo root" >&2
    exit 1
fi
EXAMPLES_DIR="${REPO_ROOT}/examples"

# --- locate / build the binary ---
TXCO="${TXCO:-}"
BUILT_TXCO=
if [[ -z "${TXCO}" ]]; then
    BUILT_TXCO="$(mktemp -t txco-examples-smoke-XXXXXX)"
    echo "==> building txco binary at ${BUILT_TXCO}..."
    ( cd "${REPO_ROOT}" && go build -o "${BUILT_TXCO}" ./cmd/txco ) || {
        echo "FAIL: go build failed" >&2
        rm -f "${BUILT_TXCO}"
        exit 1
    }
    TXCO="${BUILT_TXCO}"
fi

# --- bookkeeping + cleanup ---
LOG_DIR="$(mktemp -d -t examples-smoke-logs-XXXXXX)"
DEV_PID=          # process group of the currently-running `txco dev`
DEV_WS=           # temp workspace of the currently-running example
EXIT_CODE=0
PASS=0
FAIL=0
SKIP=0
FAILED_NAMES=()
SKIPPED_NAMES=()

# Kill the current dev (whole process group — dev forks a child chassis
# and the example's app processes) and drop its temp workspace.
teardown_dev() {
    set +e
    if [[ -n "${DEV_PID}" ]]; then
        kill -TERM "-${DEV_PID}" 2>/dev/null || kill -TERM "${DEV_PID}" 2>/dev/null
        sleep 2
        kill -KILL "-${DEV_PID}" 2>/dev/null || kill -KILL "${DEV_PID}" 2>/dev/null
    fi
    # Belt + suspenders: stamp out anything left on our private ports.
    for p in "${ADMIN_PORT}" "${WEB_PORT}"; do
        local pid
        pid="$(lsof -ti tcp:"${p}" 2>/dev/null || true)"
        [[ -n "${pid}" ]] && kill -KILL "${pid}" 2>/dev/null
    done
    wait 2>/dev/null
    [[ -n "${DEV_WS}" ]] && rm -rf "${DEV_WS}"
    DEV_PID=
    DEV_WS=
    set -uo pipefail
}

cleanup() {
    set +e
    teardown_dev
    [[ -n "${BUILT_TXCO}" ]] && rm -f "${BUILT_TXCO}"
    if [[ "${EXIT_CODE}" == 0 ]]; then
        rm -rf "${LOG_DIR}"
    else
        echo "logs preserved at: ${LOG_DIR}"
    fi
    exit "${EXIT_CODE}"
}
trap cleanup EXIT INT TERM

pass() { echo "  ✅ $*"; }
note() { echo "  • $*"; }
softfail() { echo "  ❌ $*" >&2; }

preflight_port_free() {
    local port="$1" what="$2"
    if lsof -nP -iTCP:"${port}" -sTCP:LISTEN 2>/dev/null | grep -q LISTEN; then
        echo "FAIL: ${what} port ${port} is already in use. Stop the stale process (lsof -i :${port}) and re-run." >&2
        EXIT_CODE=1
        exit 1
    fi
}
preflight_port_free "${ADMIN_PORT}" "harness admin"
preflight_port_free "${WEB_PORT}" "harness web"

# port_busy <port> → 0 if something is LISTENing, else 1.
port_busy() {
    lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | grep -q LISTEN
}

# app_ports <workspace> → distinct localhost ports the example's apps use
# (parsed from txco.yaml), excluding the control-plane port 8081.
app_ports() {
    local ws="$1"
    [[ -f "${ws}/txco.yaml" ]] || return 0
    grep -oE 'localhost:[0-9]+' "${ws}/txco.yaml" 2>/dev/null \
        | cut -d: -f2 | sort -u | grep -v '^8081$' || true
}

# emit_probe <probe.json> — print the probe as tab-separated lines:
#   BIND<TAB>host<TAB>stack
#   CHECK<TAB>name<TAB>method<TAB>host<TAB>path<TAB>status<TAB>contains<TAB>body
# (body is the only field that can be empty; none of the fields contain tabs.)
emit_probe() {
    python3 - "$1" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
for b in p.get("bind", []):
    print("BIND\t%s\t%s" % (b["host"], b["stack"]))
for c in p.get("checks", []):
    print("CHECK\t%s\t%s\t%s\t%s\t%s\t%s\t%s" % (
        c.get("name", ""),
        c.get("method", "GET"),
        c.get("host", "localhost"),
        c.get("path", "/"),
        c.get("status", 200),
        c.get("contains", ""),
        c.get("body", ""),
    ))
PY
}

# run_checks <name> <probe.json> — boot dev for the example workspace in
# $DEV_WS, apply binds, run checks. Sets a non-empty $RUN_ERR on failure.
RUN_ERR=
run_checks() {
    local name="$1" probe="$2"
    RUN_ERR=
    local log="${LOG_DIR}/${name}.dev.log"

    # Redirect the dev target at our private admin port so the parent
    # dials the chassis it spawned, not the user's :8081.
    sed -i.bak "s#localhost:8081#localhost:${ADMIN_PORT}#g" "${DEV_WS}/txco.yaml" 2>/dev/null
    rm -f "${DEV_WS}/txco.yaml.bak"

    ( cd "${DEV_WS}" && "${TXCO}" dev \
        --chassis-addr ":${ADMIN_PORT}" \
        --web-addr ":${WEB_PORT}" \
        --watch=false ) >"${log}" 2>&1 &
    DEV_PID=$!

    local up=
    for _ in $(seq 1 40); do
        if curl -fsS "${ADMIN_URL}/healthz" >/dev/null 2>&1; then up=1; break; fi
        sleep 1
    done
    if [[ -z "${up}" ]]; then
        RUN_ERR="chassis did not become healthy on ${ADMIN_URL} in 40s (see ${log})"
        return
    fi
    # Give dev's startup apply a moment to push + activate the stacks.
    sleep 2

    # Apply hostname bindings + run checks from the probe.
    local kind a b c d e f g
    while IFS=$'\t' read -r kind a b c d e f g; do
        case "${kind}" in
        BIND)
            # a=host b=stack
            local code
            code="$(curl -sS -o /dev/null -w '%{http_code}' \
                -X POST "${ADMIN_URL}/v1/tenants/default/hostnames" \
                -H 'Content-Type: application/json' \
                -d "{\"hostname\":\"${a}\",\"stack\":\"${b}\"}" 2>/dev/null)"
            if [[ "${code}" != "201" && "${code}" != "200" && "${code}" != "409" ]]; then
                RUN_ERR="bind ${a} → ${b} failed (HTTP ${code})"
                return
            fi
            ;;
        CHECK)
            # a=name b=method c=host d=path e=status f=contains g=body
            local body_file out status
            body_file="${LOG_DIR}/${name}.body"
            local -a curlargs=(-sS -o "${body_file}" -w '%{http_code}'
                -X "${b}" "${WEB_URL}${d}" -H "Host: ${c}")
            if [[ -n "${g}" ]]; then
                curlargs+=(-H 'Content-Type: application/json' -d "${g}")
            fi
            status="$(curl "${curlargs[@]}" 2>/dev/null)"
            out="$(cat "${body_file}" 2>/dev/null)"
            rm -f "${body_file}"
            if [[ "${status}" != "${e}" ]]; then
                RUN_ERR="check '${a}': got HTTP ${status}, want ${e}"
                return
            fi
            if [[ -n "${f}" ]] && ! grep -qF -- "${f}" <<<"${out}"; then
                RUN_ERR="check '${a}': body missing substring '${f}'"
                return
            fi
            pass "check '${a}' → HTTP ${status}, body contains '${f}'"
            ;;
        esac
    done < <(emit_probe "${probe}")
}

# run_example <name> <dir>
run_example() {
    local name="$1" dir="$2"
    local probe="${dir}/probe.json"
    echo
    echo "════════ ${name} ════════"

    # 1) Baseline — hermetic parse/ref/validate.
    if ! "${TXCO}" apply --dry-run "${dir}" >"${LOG_DIR}/${name}.dryrun.log" 2>&1; then
        softfail "baseline: apply --dry-run failed"
        sed 's/^/      /' "${LOG_DIR}/${name}.dryrun.log" | tail -6 >&2
        FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
        return
    fi
    pass "baseline: apply --dry-run validated"

    # No checks → baseline-only example, done.
    local n_checks
    n_checks="$(emit_probe "${probe}" | grep -c '^CHECK' || true)"
    if [[ "${n_checks}" == "0" ]]; then
        note "baseline-only (no HTTP checks)"
        PASS=$((PASS + 1))
        return
    fi

    # 2) Checks — needs a booted chassis (+ the example's apps).
    DEV_WS="$(mktemp -d -t "examples-smoke-${name}-XXXXXX")"
    cp -a "${dir}/." "${DEV_WS}/"
    # Drop committed dev runtime state so this is a true cold boot
    # (these dirs are gitignored; absent in CI, present in local clones).
    rm -rf "${DEV_WS}/.txco" "${DEV_WS}/chassis"
    rm -f "${DEV_WS}/probe.json"

    # If the example's app ports are taken on this machine, dev's app
    # health checks would fail — skip CHECKS (baseline already passed).
    local busy=
    for p in $(app_ports "${DEV_WS}"); do
        if port_busy "${p}"; then busy="${p}"; break; fi
    done
    if [[ -n "${busy}" ]]; then
        note "app port ${busy} in use → skipping HTTP checks (baseline passed)"
        SKIP=$((SKIP + 1)); SKIPPED_NAMES+=("${name}")
        teardown_dev
        return
    fi

    run_checks "${name}" "${probe}"
    teardown_dev

    if [[ -n "${RUN_ERR}" ]]; then
        softfail "${RUN_ERR}"
        FAIL=$((FAIL + 1)); FAILED_NAMES+=("${name}")
    else
        PASS=$((PASS + 1))
    fi
}

echo "txco:     ${TXCO}"
echo "examples: ${EXAMPLES_DIR}"
echo "ports:    admin :${ADMIN_PORT}, web :${WEB_PORT}"

for dir in "${EXAMPLES_DIR}"/*/; do
    [[ -f "${dir}/probe.json" ]] || continue
    run_example "$(basename "${dir}")" "${dir%/}"
done

echo
echo "──────── summary ────────"
echo "  passed:  ${PASS}"
echo "  skipped: ${SKIP}${SKIPPED_NAMES:+ (${SKIPPED_NAMES[*]})}"
echo "  failed:  ${FAIL}${FAILED_NAMES:+ (${FAILED_NAMES[*]})}"
if [[ "${FAIL}" -gt 0 ]]; then
    echo
    echo "❌ ${FAIL} example(s) failed"
    EXIT_CODE=1
else
    echo
    echo "✅ all examples passed (${SKIP} skipped)"
fi
