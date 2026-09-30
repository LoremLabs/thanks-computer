BIN=/opt/txco/bin/txco
DATA=/var/lib/txco
ADMIN="127.0.0.1:$ADMIN_PORT"
WEB="127.0.0.1:$WEB_PORT"
pid="$(cat "$DATA/node.pid" 2>/dev/null || true)"
echo "admin /healthz : $(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://$ADMIN/healthz")"
echo "web   /healthz : $(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://$WEB/healthz")"
echo "release        : $("$BIN" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p')"
echo "installed tag  : $(cat "$DATA/release" 2>/dev/null || true)"
echo "pid            : ${pid:-none}"
if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
  echo "age            : $(ps -o etimes= -p "$pid" | tr -d ' ') s"
  echo "memory (rss)   : $(( $(ps -o rss= -p "$pid" | tr -d ' ') / 1024 )) MB"
fi
echo "binary         : $(du -sh "$BIN" 2>/dev/null | cut -f1)"
echo "data directory : $(du -sh "$DATA" 2>/dev/null | cut -f1)"
echo "grant socket   : $(ls "${TMPDIR:-/tmp}"/txco-*/g.sock 2>/dev/null | head -n1 || true)"
# Enrollment must be CLOSED once setup is done: the chassis runs without a
# secret, so the route answers 404 whatever is sent.
echo "enroll route   : $(curl -s -m 3 -o /dev/null -w '%{http_code}' -X POST "http://$ADMIN/auth/dev/enroll" -H 'X-Txco-Enroll-Secret: guess' -H 'Content-Type: application/json' --data '{}') (404 = closed)"
echo "setup log      :"
sed 's/^/  /' /tmp/txco-node-setup.log 2>/dev/null || echo "  (none)"
