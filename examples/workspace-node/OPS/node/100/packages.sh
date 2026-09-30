BIN=/opt/txco/bin/txco
DATA=/var/lib/txco
ADMIN="http://127.0.0.1:$ADMIN_PORT"
export TXCO_HOME="$DATA/home"
W="$DATA/stacks"
# The release this node runs: the example package is taken from the same tag.
VER="$("$BIN" version 2>/dev/null | sed -n 's/.*"version": *"\([^"]*\)".*/\1/p')"
mkdir -p "$W" && cd "$W" || exit 1
step() { echo; echo "== $*"; }
run() { "$@"; echo "   exit=$?"; }

# 1. A registry package. hello-world is rules only. --require-signature first:
#    whether the published package is signed is one of the things to learn.
step "install hello-world from the registry, requiring a signature"
if "$BIN" install hello-world --as hello --force --require-signature; then
  echo "   exit=0 (signed)"
else
  echo "   exit=$? — again without --require-signature"
  run "$BIN" install hello-world --as hello --force
fi

# 2. A package that bundles a compute (classify.js), taken from GitHub because
#    none is published. `txco apply` builds a bundled .js to wasm unless the
#    package ships the .wasm beside it; this is where a stock image may fall short.
step "install support-basic from GitHub (it bundles a compute)"
run "$BIN" install "github:LoremLabs/thanks-computer@v$VER/examples/packages/support-basic" --as support --force
# The package names two external operations it needs. `apply` refuses the whole
# workspace until they resolve, so give them somewhere to point. Nothing below
# sends the support stack a request, so nothing dials them.
grep -q '^operations:' txco.yaml 2>/dev/null || cat >> txco.yaml <<'YAML'
operations:
  AUDIT:
    url: "https://audit.example.com/op"
  NOTIFY:
    url: "https://notify.example.com/op"
YAML
step "the node workspace's txco.yaml"
sed 's/^/   /' txco.yaml

# 3. A stack written on the node: the inspector POST /call asks. It answers
#    from the node's own machine, through the node's own local workspace
#    provider — a node stack acting where it runs.
step "write the _inspect stack"
mkdir -p OPS/_inspect/100 OPS/_inspect/200
cat > OPS/_inspect/100/where.txcl <<'TXCL'
# Ask this machine what it is. The node's workspace provider is `local`: the
# command runs on the node itself.
WHEN @src == "inspect" && @inspect.stack == "node"
  EXEC "workspace://self/exec"
    WITH command = "uname -srm; hostname",
         into = "_where"
TXCL
cat > OPS/_inspect/200/card.txcl <<'TXCL'
# Answer the inspect request with a card. `._inspect.card` is the reply the
# inspect inlet looks for.
WHEN @src == "inspect" && @inspect.stack == "node"
  EMIT ._inspect.card = &object(
         "title", "Node",
         "sections", &array(
           &object("title", "Request",
                   "rows", &array(
                     &array("Noun", @inspect.noun),
                     &array("Id", @inspect.id))),
           &object("title", "Answered by",
                   "rows", &array(
                     &array("Stack", "_inspect, written on the node"),
                     &array("Machine", ._where.stdout),
                     &array("Provider error", .workspace.error.code))))),
       @halt = true
TXCL
echo "   written"

step "apply, signed with the node-local key"
t0="$(date +%s)"
run "$BIN" apply --addr "$ADMIN" --profile node --yes
echo "   took $(( $(date +%s) - t0 )) s"

step "bundled computes, built on the node"
find "$W" "$HOME/.cache" "$TXCO_HOME" \( -name '*.wasm' -o -name 'javy*' \) -type f 2>/dev/null | while read -r f; do
  echo "   $(wc -c < "$f" | tr -d ' ') bytes  $f"
done

step "hostnames the node minted"
hosts="$("$BIN" auth tenant hostnames list --profile node 2>&1)"
echo "$hosts" | sed 's/^/   /'

# Only hello is asked: it is rules alone. The support stack would dial the
# two operations above. `apply` returns when a version is ACTIVATED; the
# chassis serves it a moment later, when it next reloads its stacks. So ask
# until it answers, and say how long that took.
step "the data plane, on the node's loopback port"
for h in $(echo "$hosts" | grep -o 'hello-[a-z0-9]*\.localhost' | sort -u); do
  t0="$(date +%s.%N)"
  for i in $(seq 1 50); do
    code="$(curl -s -m 10 -o /tmp/txco-node-dp.out -w '%{http_code}' -H "Host: $h:$WEB_PORT" "http://127.0.0.1:$WEB_PORT/")"
    [ "$code" = 200 ] && break
    sleep 0.2
  done
  echo "   $h → HTTP $code after $(awk -v a="$t0" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f", b - a }') s: $(head -c 120 /tmp/txco-node-dp.out | tr '\n' ' ')"
done
exit 0
