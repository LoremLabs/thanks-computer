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
#      validate → activate → route → execute. Checks that hit an async
#      (`mode = "async"`) pipeline get a 202 + continuation poll URL;
#      the harness follows it to the terminal result before asserting,
#      so a probe just declares the final outcome (see CONTINUATION_* ).
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

# --- async-continuation polling ---
# An example op with `WITH mode = "async"` (e.g. hello-world/150's
# research → the worker app on :9009) suspends the pipeline: the inlet
# returns 202 + `Location: <path>?_txc.continuation=<rcid>` and the
# worker calls back later. The poll URL returns 202 while running, 200
# (the rendered result) once the worker completes, 502 on failure, 404
# if unknown — see OPS/_sys/txc-continuation. So a check that declares
# the FINAL outcome (status 200, body substring) must follow the 202 to
# its conclusion. The smoke shortens the worker's job via WORKER_JOB_MS
# (see run_checks), so the callback lands in well under a second; we
# poll every 1s and keep a generous budget as a safety net (e.g. if the
# override doesn't take and the worker falls back to its ~20-26s default).
CONTINUATION_BUDGET_S=45
CONTINUATION_POLL_S=1

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
    ( cd "${REPO_ROOT}" && go build -tags sqlite_fts5 -o "${BUILT_TXCO}" ./cmd/txco ) || {
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
DEV_APP_PORTS=    # the current example's app ports (from txco.yaml)
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
    # Belt + suspenders: stamp out anything left on our private ports AND
    # the example's app ports. Without the app ports, a Node app that
    # outlives its parent dev's process group (e.g. the :4100 api server)
    # lingers past teardown — and since several examples hardcode the same
    # app ports (4100/9009/…), the NEXT example sees the port busy and
    # skips its checks. Killing them here keeps sequential runs isolated.
    for p in "${ADMIN_PORT}" "${WEB_PORT}" ${DEV_APP_PORTS}; do
        local pid
        pid="$(lsof -ti tcp:"${p}" 2>/dev/null || true)"
        [[ -n "${pid}" ]] && kill -KILL "${pid}" 2>/dev/null
    done
    wait 2>/dev/null
    [[ -n "${DEV_WS}" ]] && rm -rf "${DEV_WS}"
    DEV_PID=
    DEV_WS=
    DEV_APP_PORTS=
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

# fire_once <method> <url> <host> <body> <body_file> <hdr_file> → echoes
# the HTTP status code. Response body lands in body_file, headers in
# hdr_file (so the caller can read Location for continuation polling).
# No -L: we follow continuations explicitly, not HTTP redirects.
fire_once() {
    local method="$1" url="$2" host="$3" body="$4" body_file="$5" hdr_file="$6"
    local -a args=(-sS -o "${body_file}" -D "${hdr_file}" -w '%{http_code}'
        -X "${method}" "${url}" -H "Host: ${host}")
    if [[ -n "${body}" ]]; then
        args+=(-H 'Content-Type: application/json' -d "${body}")
    fi
    curl "${args[@]}" 2>/dev/null
}

# continuation_poll_url <hdr_file> <body_file> <base_path> → echoes the
# relative poll URL for a 202 continuation, or empty if none found.
#
# Two 202 shapes exist (chassis/processor):
#   - continuable / same-scope barrier (emitContinuation202): sets a
#     `Location:` header + body {"status":"running","continuation":…}.
#   - async / deferred-join suspend: NO Location header; the rcid is
#     only in the body, {"status":"waiting","continuation":"rc_…"}.
# So prefer the Location header, then fall back to the body's
# `continuation` field combined with the request's base path
# (<path>?_txc.continuation=<rcid> — the poll form the chassis honors).
continuation_poll_url() {
    local hdr="$1" body="$2" base="$3" loc rcid sep
    loc="$(grep -i '^location:' "${hdr}" 2>/dev/null | head -1 \
        | sed 's/^[Ll]ocation:[[:space:]]*//' | tr -d '\r' \
        | sed 's/[[:space:]]*$//')"
    if [[ -n "${loc}" ]]; then
        printf '%s' "${loc}"
        return
    fi
    rcid="$(grep -oE '"continuation"[[:space:]]*:[[:space:]]*"[^"]+"' "${body}" 2>/dev/null \
        | head -1 | sed -E 's/.*"continuation"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
    if [[ -n "${rcid}" ]]; then
        sep='?'
        [[ "${base}" == *\?* ]] && sep='&'
        printf '%s%s_txc.continuation=%s' "${base}" "${sep}" "${rcid}"
    fi
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

    # TXCO_DEBUG_BREAKPOINTS=false: `txco dev` enables breakpoints by
    # default, and the inlet then stamps _txc.flag_breakpoint on every
    # request — which the processor uses to DISABLE response streaming
    # (chassis/processor/processor.go: "Breakpoints pre-empt streaming
    # entirely"). The smoke verifies examples as they behave in
    # PRODUCTION (where breakpoints are off), so we force them off here.
    # Without this, stream-demo's /stream returns a buffered final body
    # ("done.") instead of the streamed chunks ("starting…" first), and
    # its probe can never match. startChassis sets dev defaults
    # set-if-missing, so this parent env var wins.
    # WORKER_JOB_MS=500: example async workers (APPS/worker) default to a
    # ~20-26s simulated job to look realistic for a human; that's dead
    # wait for the smoke. The worker reads WORKER_JOB_MS as an override
    # (dev passes its env down to spawned apps), so a short value still
    # exercises the full 202→poll→200 continuation path in ~1s. Harmless
    # for examples without a worker.
    ( cd "${DEV_WS}" && \
        TXCO_DEBUG_BREAKPOINTS=false \
        WORKER_JOB_MS=500 \
        "${TXCO}" dev \
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
            local body_file hdr_file out status
            body_file="${LOG_DIR}/${name}.body"
            hdr_file="${LOG_DIR}/${name}.hdr"
            status="$(fire_once "${b}" "${WEB_URL}${d}" "${c}" "${g}" "${body_file}" "${hdr_file}")"

            # Follow an async continuation to its terminal result. A
            # pipeline with a `mode = "async"` op returns 202 + a poll
            # URL in Location; the poll itself returns 202 while the
            # worker runs, then a terminal status (200 result / 502
            # failed / 404 unknown). The probe declares the FINAL
            # outcome, so the harness transparently drives the poll to
            # completion. Each poll is a GET (regardless of the original
            # method) with no body.
            if [[ "${status}" == "202" ]]; then
                local poll_url deadline
                # Derive the poll URL ONCE from the first 202 (the rcid is
                # stable for the run); subsequent polls reuse it, so we
                # don't depend on every interim 202 echoing the rcid.
                poll_url="$(continuation_poll_url "${hdr_file}" "${body_file}" "${d}")"
                if [[ -z "${poll_url}" ]]; then
                    RUN_ERR="check '${a}': got 202 but no continuation (Location header or body rcid) to poll"
                    return
                fi
                deadline=$(( $(date +%s) + CONTINUATION_BUDGET_S ))
                while [[ "${status}" == "202" ]]; do
                    if (( $(date +%s) >= deadline )); then
                        RUN_ERR="check '${a}': continuation did not resolve within ${CONTINUATION_BUDGET_S}s (still 202)"
                        return
                    fi
                    sleep "${CONTINUATION_POLL_S}"
                    status="$(fire_once GET "${WEB_URL}${poll_url}" "${c}" "" "${body_file}" "${hdr_file}")"
                done
                note "check '${a}': followed async continuation → HTTP ${status}"
            fi

            out="$(cat "${body_file}" 2>/dev/null)"
            rm -f "${body_file}" "${hdr_file}"
            if [[ "${status}" != "${e}" ]]; then
                RUN_ERR="check '${a}': got HTTP ${status}, want ${e}"
                return
            fi
            if [[ -n "${f}" ]] && ! grep -qF -- "${f}" <<<"${out}"; then
                # Show a snippet of what we actually got — a missing
                # substring is usually a stale probe expectation, and the
                # real body tells you what to assert instead.
                local snippet
                snippet="$(printf '%s' "${out}" | tr -d '\r\n' | cut -c1-160)"
                RUN_ERR="check '${a}': body missing substring '${f}' (got: ${snippet})"
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

    # Record the example's app ports so teardown can free them (some
    # examples share ports like :4100, and a lingering app would make the
    # next example skip).
    DEV_APP_PORTS="$(app_ports "${DEV_WS}" | tr '\n' ' ')"

    # If the example's app ports are taken on this machine, dev's app
    # health checks would fail — skip CHECKS (baseline already passed).
    local busy=
    for p in ${DEV_APP_PORTS}; do
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
