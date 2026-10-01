#!/usr/bin/env bash
# scripts/registry-token-e2e.sh
#
# End-to-end check of per-tenant registry writes, against the REAL pieces:
# two stock Distribution registries on one store (one demanding tokens, one
# read-only and open), a Caddy in front routing reads to the open one and
# everything else to the other, and a dev chassis as the token service.
# It is the hosted registry's topology on localhost:
#
#   txco ──► caddy :5555 ──┬─ GET/HEAD ────────────► registry-ro :15001 (no auth, read-only)
#                          └─ everything else ─────► registry    :15000 (token auth)
#   txco ──► chassis :18281  POST /v1/tenants/{t}/registry/token   (signs the token)
#
# What it proves that `go test` cannot: that a token this chassis mints is
# accepted by the registry's own verifier, that the registry enforces the
# repository in it, and that the read path needs no token and no chassis.
#
# Usage:
#   scripts/registry-token-e2e.sh                       # registry:2.8.3
#   REGISTRY_IMAGE_TAG=3.1.2 scripts/registry-token-e2e.sh
#   TXCO=/path/to/txco scripts/registry-token-e2e.sh    # a pre-built binary
#
# Needs docker, caddy, openssl, curl, python3. Exit 0 on a full pass; a
# failed run keeps its logs.

set -uo pipefail

REGISTRY_IMAGE_TAG="${REGISTRY_IMAGE_TAG:-2.8.3}"
EDGE_PORT=5555
AUTH_PORT=15000
RO_PORT=15001
CHASSIS_PORT=18281
WEB_PORT=18280
REG="localhost:${EDGE_PORT}"
CHASSIS_URL="http://localhost:${CHASSIS_PORT}"
ISSUER="txco-e2e"
TENANT=default
RUN="txco-e2e-$$"

for bin in docker caddy openssl curl python3; do
    command -v "${bin}" >/dev/null 2>&1 || { echo "FAIL: ${bin} not found" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "FAIL: docker is not running" >&2; exit 1; }
for p in "${EDGE_PORT}" "${AUTH_PORT}" "${RO_PORT}" "${CHASSIS_PORT}" "${WEB_PORT}"; do
    if lsof -nP -iTCP:"${p}" -sTCP:LISTEN 2>/dev/null | grep -q LISTEN; then
        echo "FAIL: port ${p} is already in use (lsof -nP -iTCP:${p} -sTCP:LISTEN)" >&2
        exit 1
    fi
done

# --- the binary ---
TXCO="${TXCO:-}"
BUILT_TXCO=
if [[ -z "${TXCO}" ]]; then
    REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)"
    [[ -n "${REPO_ROOT}" ]] || { echo "FAIL: not inside a git repo and TXCO not set" >&2; exit 1; }
    BUILT_TXCO="$(mktemp -t txco-e2e-XXXXXX)"
    echo "==> building txco at ${BUILT_TXCO}"
    ( cd "${REPO_ROOT}" && go build -tags sqlite_fts5 -o "${BUILT_TXCO}" ./cmd/txco ) || {
        echo "FAIL: go build failed" >&2; rm -f "${BUILT_TXCO}"; exit 1; }
    TXCO="${BUILT_TXCO}"
fi

# --- sandbox + cleanup ---
WORK="$(mktemp -d -t registry-token-e2e-XXXXXX)"
LOGS="${WORK}/logs"; mkdir -p "${LOGS}"
CHASSIS_PID=; CADDY_PID=; EXIT_CODE=0

stop_chassis() {
    [[ -n "${CHASSIS_PID}" ]] || return 0
    kill -TERM "-${CHASSIS_PID}" 2>/dev/null || kill -TERM "${CHASSIS_PID}" 2>/dev/null
    sleep 1
    kill -KILL "-${CHASSIS_PID}" 2>/dev/null || kill -KILL "${CHASSIS_PID}" 2>/dev/null
    for p in "${CHASSIS_PORT}" "${WEB_PORT}"; do
        pid="$(lsof -ti tcp:"${p}" -sTCP:LISTEN 2>/dev/null || true)"
        [[ -n "${pid}" ]] && kill -KILL ${pid} 2>/dev/null
    done
    CHASSIS_PID=
}
cleanup() {
    set +e
    stop_chassis
    [[ -n "${CADDY_PID}" ]] && kill -TERM "${CADDY_PID}" 2>/dev/null
    if [[ "${EXIT_CODE}" != 0 ]]; then
        docker logs "${RUN}-auth" >"${LOGS}/registry-auth.log" 2>&1
        docker logs "${RUN}-ro" >"${LOGS}/registry-ro.log" 2>&1
    fi
    docker rm -f "${RUN}-auth" "${RUN}-ro" >/dev/null 2>&1
    docker volume rm "${RUN}" >/dev/null 2>&1
    wait 2>/dev/null
    [[ -n "${BUILT_TXCO}" ]] && rm -f "${BUILT_TXCO}"
    if [[ "${EXIT_CODE}" == 0 ]]; then
        rm -rf "${WORK}"
    else
        echo "logs preserved at: ${LOGS}"
    fi
    exit "${EXIT_CODE}"
}
trap cleanup EXIT INT TERM

fail() { echo "❌ FAIL: $*" >&2; EXIT_CODE=1; exit 1; }
pass() { echo "✅ $*"; }

# http_code METHOD URL [curl args…] → the status code
http_code() {
    local method="$1" url="$2"; shift 2
    curl -s -o /dev/null -w '%{http_code}' -X "${method}" "$@" "${url}"
}
expect_code() {
    local want="$1" what="$2"; shift 2
    local got; got="$(http_code "$@")"
    [[ "${got}" == "${want}" ]] || fail "${what}: HTTP ${got}, want ${want}"
    pass "${what} (${want})"
}
# mint SCOPE… → a token from the chassis, for curl-level checks
mint() {
    local scopes=""
    for s in "$@"; do scopes+="\"${s}\","; done
    curl -fsS -X POST "${CHASSIS_URL}/v1/tenants/${TENANT}/registry/token" \
        -H 'Content-Type: application/json' \
        -d "{\"service\":\"${REG}\",\"scopes\":[${scopes%,}]}" |
        python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])'
}

echo "work dir: ${WORK}   registry image: registry:${REGISTRY_IMAGE_TAG}"
echo

# ---------------------------------------------------------------
echo "==> [1/8] token key + certificate (throwaway)"
# What an operator does once: a P-256 key and a self-signed certificate.
# The certificate is the registry's whole trust in the chassis.
openssl ecparam -name prime256v1 -genkey -noout -out "${WORK}/token.key" 2>/dev/null ||
    fail "openssl: key"
openssl req -x509 -new -key "${WORK}/token.key" -days 30 \
    -subj "/CN=txco registry token (e2e)" -out "${WORK}/root.crt" 2>/dev/null ||
    fail "openssl: certificate"
pass "key + certificate"

# ---------------------------------------------------------------
echo "==> [2/8] two registries on one store"
cat > "${WORK}/config.yml" <<YAML
version: 0.1
log: {level: info}
storage:
  filesystem: {rootdirectory: /var/lib/registry}
  delete: {enabled: true}
http:
  addr: :5000
  host: http://${REG}
  secret: e2e-not-a-secret
auth:
  token:
    realm: ${CHASSIS_URL}/v1/registry/token
    service: ${REG}
    issuer: ${ISSUER}
    rootcertbundle: /etc/registry-token/root.crt
YAML
cat > "${WORK}/config-ro.yml" <<YAML
version: 0.1
log: {level: info}
storage:
  filesystem: {rootdirectory: /var/lib/registry}
  delete: {enabled: false}
  maintenance:
    uploadpurging: {enabled: false}
    readonly: {enabled: true}
http:
  addr: :5000
  host: http://${REG}
  secret: e2e-not-a-secret
YAML
docker volume create "${RUN}" >/dev/null || fail "docker volume"
# Each config is mounted at both paths an image reads: 2.x's and 3.x's.
docker run -d --name "${RUN}-auth" -p "127.0.0.1:${AUTH_PORT}:5000" \
    -v "${RUN}:/var/lib/registry" \
    -v "${WORK}/config.yml:/etc/docker/registry/config.yml:ro" \
    -v "${WORK}/config.yml:/etc/distribution/config.yml:ro" \
    -v "${WORK}/root.crt:/etc/registry-token/root.crt:ro" \
    "registry:${REGISTRY_IMAGE_TAG}" >/dev/null || fail "start the token-auth registry"
docker run -d --name "${RUN}-ro" -p "127.0.0.1:${RO_PORT}:5000" \
    -v "${RUN}:/var/lib/registry:ro" \
    -v "${WORK}/config-ro.yml:/etc/docker/registry/config.yml:ro" \
    -v "${WORK}/config-ro.yml:/etc/distribution/config.yml:ro" \
    "registry:${REGISTRY_IMAGE_TAG}" >/dev/null || fail "start the read-only registry"
# `GET /` is the health handler, outside auth: the same probe works for both.
for port in "${AUTH_PORT}" "${RO_PORT}"; do
    curl -fs --retry 20 --retry-delay 1 --retry-all-errors -o /dev/null "http://127.0.0.1:${port}/" ||
        fail "registry on :${port} did not come up"
done
expect_code 401 "token-auth registry challenges /v2/" GET "http://127.0.0.1:${AUTH_PORT}/v2/"
expect_code 200 "read-only registry answers /v2/ openly" GET "http://127.0.0.1:${RO_PORT}/v2/"

# ---------------------------------------------------------------
echo "==> [3/8] edge"
cat > "${WORK}/Caddyfile" <<CADDY
{
	auto_https off
	admin off
}
http://${REG} {
	handle /.well-known/txco-registry.json {
		header Content-Type application/json
		respond \`{"token_service":"${CHASSIS_URL}"}\` 200
	}
	@read {
		method GET HEAD
		not path */blobs/uploads/* /v2/_catalog
	}
	handle @read {
		reverse_proxy 127.0.0.1:${RO_PORT}
	}
	handle {
		reverse_proxy 127.0.0.1:${AUTH_PORT}
	}
}
CADDY
caddy run --config "${WORK}/Caddyfile" --adapter caddyfile >"${LOGS}/caddy.log" 2>&1 &
CADDY_PID=$!
curl -fs --retry 20 --retry-delay 1 --retry-all-errors -o /dev/null "http://${REG}/v2/" ||
    fail "edge did not come up"
pass "edge up: anonymous GET /v2/ is 200"

# ---------------------------------------------------------------
echo "==> [4/8] chassis as the token service"
start_chassis() {
    ( cd "${WORK}/ws" &&
        TXCO_REGISTRY_TOKEN_KEY="${WORK}/token.key" \
        TXCO_REGISTRY_TOKEN_CERT="${WORK}/root.crt" \
        TXCO_REGISTRY_TOKEN_ISSUER="${ISSUER}" \
        TXCO_REGISTRY_TOKEN_SERVICE="${REG}" \
        TXCO_REGISTRY_TOKEN_PLATFORM_NAMESPACES="txco=tnt_e2e_platform" \
        exec "${TXCO}" dev --chassis-addr ":${CHASSIS_PORT}" --web-addr ":${WEB_PORT}" --watch=false ) >>"${LOGS}/chassis.log" 2>&1 &
    CHASSIS_PID=$!
    for _ in $(seq 1 30); do
        curl -fsS "${CHASSIS_URL}/healthz" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}
mkdir -p "${WORK}/ws/OPS"
cat > "${WORK}/ws/txco.yaml" <<YAML
target: dev
targets:
  dev:
    chassis: ${CHASSIS_URL}
YAML
start_chassis || fail "chassis did not answer on ${CHASSIS_URL}"
grep -q "registry-token enabled" "${LOGS}/chassis.log" || fail "the chassis did not enable the token endpoint"
grep "registry-token enabled" "${LOGS}/chassis.log" | grep -Eq 'platform_namespaces[^0-9]*1([^0-9]|$)' ||
    fail "the chassis did not read the pinned platform namespace from its environment"
pass "chassis up, token endpoint enabled, one platform namespace pinned"

# The CLI under test: no profile, no docker credentials, no trust config.
export TXCO_HOME="${WORK}/home" DOCKER_CONFIG="${WORK}/docker" TXCO_NO_KEY_DISCOVERY=1
mkdir -p "${TXCO_HOME}" "${DOCKER_CONFIG}"
unset TXCO_PROFILE TXCO_PRIVATE_KEY_PATH TXCO_OCI_USERNAME TXCO_OCI_PASSWORD

# ---------------------------------------------------------------
echo "==> [5/8] what the registry refuses by itself"
CHALLENGE="$(curl -s -o /dev/null -D - -X POST "http://${REG}/v2/${TENANT}/hello/blobs/uploads/" | tr -d '\r' | grep -i '^www-authenticate:')"
# registry:2.8 lists the two actions in either order.
echo "${CHALLENGE}" | grep -Eq "scope=\"repository:${TENANT}/hello:(pull,push|push,pull)\"" ||
    fail "an unauthenticated upload was not challenged for repository:${TENANT}/hello:pull,push (got: ${CHALLENGE})"
pass "an unauthenticated upload is challenged for exactly that repository"
expect_code 401 "the catalogue is not anonymous" GET "http://${REG}/v2/_catalog"
expect_code 401 "an upload-status read is not served by the open instance" GET "http://${REG}/v2/${TENANT}/hello/blobs/uploads/00000000-0000-0000-0000-000000000000"
expect_code 405 "a write sent straight to the read-only registry" POST "http://127.0.0.1:${RO_PORT}/v2/${TENANT}/hello/blobs/uploads/"

# ---------------------------------------------------------------
echo "==> [6/8] publish, as the tenant"
( cd "${WORK}" && "${TXCO}" package init hello "${WORK}/pkg" ) >"${LOGS}/init.log" 2>&1 || fail "package init (see ${LOGS}/init.log)"
"${TXCO}" package key generate >"${LOGS}/keygen.log" 2>&1 || fail "package key generate"
PUB="$(ls "${TXCO_HOME}"/keys/*.pub 2>/dev/null | head -1)"
[[ -n "${PUB}" ]] || fail "no signing public key under ${TXCO_HOME}/keys"
( cd "${WORK}/pkg" && "${TXCO}" package publish --to "oci://${REG}/${TENANT}/hello:0.1.0" --sign ) >"${LOGS}/publish.log" 2>&1 ||
    { cat "${LOGS}/publish.log" >&2; fail "publish to ${TENANT}/hello"; }
grep -q "^published oci://${REG}/${TENANT}/hello@sha256:" "${LOGS}/publish.log" || fail "publish printed no digest"
pass "txco package publish --sign → ${TENANT}/hello:0.1.0"
MINTS="$(grep -c "registry-token minted" "${LOGS}/chassis.log")"
[[ "${MINTS}" -ge 2 ]] || fail "expected a token for the package and one for its signature, the chassis minted ${MINTS}"
pass "the chassis logged ${MINTS} tokens (package, signature); none is in the log"
grep -q "eyJ" "${LOGS}/chassis.log" && fail "a token appears in the chassis log"

( cd "${WORK}/pkg" && "${TXCO}" package publish --to "oci://${REG}/nosuch/hello:0.1.0" ) >"${LOGS}/publish-nosuch.log" 2>&1 &&
    fail "publishing to a namespace that is no tenant succeeded"
grep -q 'no tenant "nosuch"' "${LOGS}/publish-nosuch.log" ||
    { cat "${LOGS}/publish-nosuch.log" >&2; fail "publishing to nosuch/ did not say there is no such tenant"; }
pass "publishing to nosuch/ is refused: no such tenant"
expect_code 404 "nosuch/hello was not created" GET "http://${REG}/v2/nosuch/hello/manifests/0.1.0"

# The chassis itself refuses another tenant's namespace and another registry.
expect_code 403 "a token for another namespace is refused by the chassis" POST "${CHASSIS_URL}/v1/tenants/${TENANT}/registry/token" \
    -H 'Content-Type: application/json' -d "{\"service\":\"${REG}\",\"scopes\":[\"repository:someone-else/hello:pull,push\"]}"
expect_code 403 "a token for another registry is refused by the chassis" POST "${CHASSIS_URL}/v1/tenants/${TENANT}/registry/token" \
    -H 'Content-Type: application/json' -d "{\"service\":\"evil.example\",\"scopes\":[\"repository:${TENANT}/hello:pull,push\"]}"

# ---------------------------------------------------------------
echo "==> [7/8] a token is for one repository, and for what it says"
TOK_A="$(mint "repository:${TENANT}/a:pull,push")" || fail "mint a token for ${TENANT}/a"
[[ -n "${TOK_A}" ]] || fail "empty token"
expect_code 202 "the token opens an upload on ${TENANT}/a" POST "http://${REG}/v2/${TENANT}/a/blobs/uploads/" -H "Authorization: Bearer ${TOK_A}"
expect_code 401 "…and not on ${TENANT}/b" POST "http://${REG}/v2/${TENANT}/b/blobs/uploads/" -H "Authorization: Bearer ${TOK_A}"
expect_code 401 "…and not on another namespace" POST "http://${REG}/v2/other/a/blobs/uploads/" -H "Authorization: Bearer ${TOK_A}"
expect_code 401 "…and does not delete" DELETE "http://${REG}/v2/${TENANT}/hello/manifests/0.1.0" -H "Authorization: Bearer ${TOK_A}"
expect_code 401 "…and does not list the catalogue" GET "http://${REG}/v2/_catalog" -H "Authorization: Bearer ${TOK_A}"
# One changed character in the middle of the signature (not the last: its
# low bits are base64 padding and decode to the same bytes).
BAD="$(python3 -c 'import sys; t=sys.argv[1]; i=len(t)-20; print(t[:i]+("A" if t[i]!="A" else "B")+t[i+1:])' "${TOK_A}")"
expect_code 401 "a tampered token is refused" POST "http://${REG}/v2/${TENANT}/a/blobs/uploads/" -H "Authorization: Bearer ${BAD}"
expect_code 200 "the published tag is still there" GET "http://${REG}/v2/${TENANT}/hello/manifests/0.1.0" \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json'

# ---------------------------------------------------------------
echo "==> [8/8] reading needs no chassis"
stop_chassis
curl -fsS "${CHASSIS_URL}/healthz" >/dev/null 2>&1 && fail "the chassis is still up"
mkdir -p "${WORK}/consumer/OPS"
( cd "${WORK}/consumer" && "${TXCO}" install "oci://${REG}/${TENANT}/hello:0.1.0" --require-signature --key "${PUB}" ) >"${LOGS}/install.log" 2>&1 ||
    { cat "${LOGS}/install.log" >&2; fail "install with the chassis down"; }
[[ -d "${WORK}/consumer/OPS/hello" ]] || fail "install did not materialise OPS/hello"
pass "txco install --require-signature works with the chassis stopped"
( cd "${WORK}/pkg" && "${TXCO}" package publish --to "oci://${REG}/${TENANT}/hello:0.1.1" ) >"${LOGS}/publish-down.log" 2>&1 &&
    fail "publish succeeded with the token service down"
pass "publish fails closed with the chassis stopped: $(tail -1 "${LOGS}/publish-down.log")"

echo
echo "registry token e2e: all checks passed (registry:${REGISTRY_IMAGE_TAG})"
