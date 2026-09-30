set -eu
[ "$(uname -s)" = Linux ] || { echo '{"ready":false,"error":"needs a Linux workspace (a real provider); the local provider would install onto your own machine"}'; exit 0; }
BIN=/opt/txco/bin/txco
DATA=/var/lib/txco
MARK=$DATA/version
LOG=/tmp/txco-node-setup.log
ADMIN="127.0.0.1:$ADMIN_PORT"
up() { curl -fsS -m 2 "http://$ADMIN/healthz" >/dev/null 2>&1; }
# The marker records what was asked for: "<REQ> <VERSION>". A POST carries both
# and re-provisions when either differs. A GET carries neither: to it, any
# marker means provisioned, so what a node runs is written in one place.
marked() {
  [ -f "$MARK" ] || return 1
  [ -z "${REQ:-}" ] || [ "$(cat "$MARK")" = "$REQ $VERSION" ]
}
keyid() { sed -n 's/.*"key_id":"\([^"]*\)".*/\1/p' "$DATA/parent-key.json" 2>/dev/null | head -n1; }
release() { "$BIN" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p'; }
# A provision that failed says so here until one succeeds. A node that was
# already running keeps running what it had, so "ready" alone would hide it.
failed() {
  e="$(tr -d '"\\' < "$DATA/setup.err" 2>/dev/null | tr '\n' ' ')"
  [ -z "$e" ] || printf ',"setup_error":"%s"' "$e"
}
# A setup that is running is reported first. While a node is provisioned
# again its old chassis keeps answering for a moment, and that is not "ready".
if pgrep -f txco-node-provision.sh >/dev/null 2>&1; then
  ph="$(tail -n1 "$LOG" 2>/dev/null | tr -d '"\\' | tr '\n' ' ')"
  echo "{\"ready\":false,\"phase\":\"${ph:-working}\"}"; exit 0
fi
# WARM: the marker is current and the admin plane answers. Nothing to do. On
# a POST that means the node runs what was asked for, so an old failure is moot.
if marked && up; then
  [ -z "${REQ:-}" ] || rm -f "$DATA/setup.err"
  echo "{\"ready\":true,\"release\":\"$(release)\",\"parent_key_id\":\"$(keyid)\",\"admin\":\"$ADMIN\"$(failed)}"; exit 0
fi
# PROVISION needs the parent's public key, and only a POST carries one. The key
# goes into a JSON body below, so it must look like base64 and nothing else.
DO=start
if ! marked; then
  case "${PARENT_PUB:-}" in
    "") echo "{\"ready\":false,\"error\":\"not provisioned: POST /setup with {parent_pub: <base64 ed25519 public key>}\"$(failed)}"; exit 0 ;;
    *[!A-Za-z0-9+/=_-]*) echo '{"ready":false,"error":"parent_pub is not base64"}'; exit 0 ;;
  esac
  DO=provision
fi
export DO
cat > /tmp/txco-node-provision.sh <<'WORKEOF'
set -eEu
BIN=/opt/txco/bin/txco
DATA=/var/lib/txco
MARK=$DATA/version
# A failure is a PHASE line for the poll, and a file GET /setup reports until
# a provision succeeds.
fail() {
  echo "PHASE error $*"
  printf '%s\n' "$*" > "$DATA/setup.err" 2>/dev/null || true
  exit 1
}
trap 'fail "setup stopped at line $LINENO"' ERR
ADMIN="127.0.0.1:$ADMIN_PORT"
WEB="127.0.0.1:$WEB_PORT"
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.2f", b - a }'; }
up() { curl -fsS -m 2 "http://$ADMIN/healthz" >/dev/null 2>&1; }
stop_node() {
  pid="$(cat "$DATA/node.pid" 2>/dev/null || true)"
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
  for i in $(seq 1 50); do up || return 0; sleep 0.2; done
  if [ -n "$pid" ]; then kill -9 "$pid" 2>/dev/null || true; fi
}
# start_node SECRET: the chassis, detached from this exec's stdio so it outlives
# it. Every data path defaults to ./chassis/data under the working directory,
# so the working directory IS the data directory. SECRET is the enroll secret
# for the one start that enrolls keys, and empty for every start after.
#
# --env=prod is the posture, not a label. The default, dev, is for a laptop:
# it answers with the chassis's private keys (_txc, _ts) in every response,
# logs every envelope in full at debug, and would leave the admin plane open
# were it not for --auth-mode=signed. A node is a deployment, so it runs as one.
# The environment also names the two database files, so a node that moves from
# one to another starts with empty stores.
#
# Tracing is off unless asked for; the node keeps full traces so its admin UI
# has something to show (/ui).
start_node() {
  cd "$DATA"
  t0="$(now)"
  TXCO_AUTH_DEV_ENROLL_SECRET="$1" setsid "$BIN" serve \
    --env=prod \
    --admin-addr "$ADMIN" --web-addr "$WEB" \
    --auth-mode=signed --personalities=web,admin,grant \
    --structured-host-suffix=.localhost \
    --trace-mode=full \
    --workspace-provider=local --workspace-allow-local \
    </dev/null >>"$DATA/node.log" 2>&1 &
  echo $! > "$DATA/node.pid"
  for i in $(seq 1 150); do up && break; sleep 0.2; done
  up || fail "the chassis did not answer /healthz"
  echo "PHASE up start_to_healthz_s=$(since "$t0")"
}
if [ "$DO" = provision ]; then
  echo "PHASE provision req=$REQ version=$VERSION"
  sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" "$DATA"
  # The project's own installer. It picks the architecture, resolves VERSION
  # ("latest", or a tag) to a release, and checks the tarball against that
  # release's checksums.txt.
  #
  # It installs into a STAGING directory, as this user and not as root. A tag
  # that does not exist, or a download that fails its checksum, must not cost
  # the node the chassis it is running: only a binary that runs is swapped in.
  t0="$(now)"
  rm -rf "$DATA/stage"
  curl -fsSL -o "$DATA/install.sh" https://get.thanks.computer/install.sh \
    || fail "could not fetch the installer"
  PREFIX="$DATA/stage" VERSION="$VERSION" bash "$DATA/install.sh" >"$DATA/install.log" 2>&1 \
    || fail "the installer could not install VERSION=$VERSION"
  "$DATA/stage/bin/txco" version >/dev/null 2>&1 \
    || fail "the binary installed for VERSION=$VERSION does not run"
  stop_node
  sudo install -d -m 0755 /opt/txco/bin
  sudo install -m 0755 "$DATA/stage/bin/txco" "$BIN"
  rm -rf "$DATA/stage"
  echo "PHASE installed release=$("$BIN" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p') install_s=$(since "$t0")"
  mkdir -p "$DATA/home"
  # Enrollment is open only while the chassis runs with a secret. Start it
  # with one, enroll the two keys, and start it again without.
  echo "PHASE enroll"
  SECRET="$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')"
  start_node "$SECRET"
  code="$(curl -sS -o "$DATA/parent-key.json.new" -w '%{http_code}' -X POST "http://$ADMIN/auth/dev/enroll" \
    -H "X-Txco-Enroll-Secret: $SECRET" -H 'Content-Type: application/json' \
    --data "{\"public_key_b64\":\"$PARENT_PUB\",\"label\":\"parent\",\"kind\":\"service\"}")"
  # 200: enrolled. 409: this key was enrolled by an earlier provision; the
  # answer names its key id either way.
  case "$code" in
    200|409) mv "$DATA/parent-key.json.new" "$DATA/parent-key.json" ;;
    *) fail "enrolling the parent key: HTTP $code" ;;
  esac
  # The node-local key: what `txco apply` on the node signs with. A node that
  # is provisioned again already has one, and the chassis still knows it. A
  # node whose stores are new (its environment changed) does not: the chassis
  # refuses the old key, so it is dropped and a fresh one enrolled.
  enroll_local() {
    TXCO_HOME="$DATA/home" "$BIN" auth bootstrap-local --url "http://$ADMIN" --secret "$SECRET" \
      --profile node --new-key --label node-local --kind service >"$DATA/bootstrap.log" 2>&1
  }
  if ! enroll_local; then
    rm -f "$DATA/home/keys/node."*
    enroll_local || fail "could not enroll the node-local key (see $DATA/bootstrap.log)"
  fi
  stop_node
  start_node ""
  printf '%s %s\n' "$REQ" "$VERSION" > "$MARK"
  rm -f "$DATA/setup.err"
  echo "PHASE provisioned"
fi
# COLD START: provisioned, and the chassis is not running.
up || start_node ""
echo "PHASE ready"
WORKEOF
setsid bash /tmp/txco-node-provision.sh </dev/null >"$LOG" 2>&1 &
echo '{"ready":false,"phase":"starting"}'
