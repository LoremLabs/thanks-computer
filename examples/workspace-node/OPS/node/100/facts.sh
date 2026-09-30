line() { printf '%-34s %s\n' "$1" "$2"; }
line "kernel" "$(uname -srm)"
line "distribution" "$(. /etc/os-release 2>/dev/null; echo "${PRETTY_NAME:-unknown}")"
line "C library" "$(ldd --version 2>/dev/null | head -n1)"
line "default user" "$(id -un) (uid $(id -u))"
line "can become root" "$(sudo -n true 2>/dev/null && echo yes || echo no)"
# A command as ANOTHER user. exec has no user parameter, so the command
# switches itself.
line "a command as another user" "$(sudo -n -u nobody id -un 2>&1 | head -n1)"
# A control group for one run (cgroup v2): make one, put a process in it,
# kill the group as a whole, remove it. Then whether it can be LIMITED: a
# child group has only the controllers its parent hands down.
cg=/sys/fs/cgroup/txco-run-probe
line "control groups" "$(stat -fc %T /sys/fs/cgroup 2>/dev/null); root has: $(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null | tr '\n' ' ')"
if sudo -n mkdir "$cg" 2>/dev/null; then
  sleep 30 & p=$!; disown "$p"
  if echo "$p" | sudo -n tee "$cg/cgroup.procs" >/dev/null 2>&1; then
    held="$(head -n1 "$cg/cgroup.procs" 2>/dev/null)"
    echo 1 | sudo -n tee "$cg/cgroup.kill" >/dev/null 2>&1
    sleep 0.3
    line "a control group for one run" "yes: created, held pid $held, killed as a group: $(kill -0 "$p" 2>/dev/null && echo no || echo yes)"
  else
    line "a control group for one run" "created, but a process could not be moved into it"
  fi
  ctl="$(cat "$cg/cgroup.controllers" 2>/dev/null | tr -d '\n')"
  line "  its controllers, as handed down" "${ctl:-(none)}"
  kill "$p" 2>/dev/null
  sudo -n rmdir "$cg" 2>/dev/null
  line "  the root can hand down" "$(echo '+memory +pids +cpu' | sudo -n tee /sys/fs/cgroup/cgroup.subtree_control >/dev/null 2>&1 && echo "yes: $(cat /sys/fs/cgroup/cgroup.subtree_control | tr '\n' ' ')" || echo "no: the write to cgroup.subtree_control was refused")"
else
  line "a control group for one run" "no: mkdir $cg refused"
fi
# UDP out, by name: DNS over UDP to a public resolver, not the workspace's own.
if command -v dig >/dev/null 2>&1; then
  line "UDP out, by name" "$(dig +time=3 +tries=1 +short example.com @dns.google 2>&1 | head -n1)"
else
  line "UDP out, by name" "$(timeout 4 bash -c 'exec 3<>/dev/udp/dns.google/53 && printf "\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01" >&3 && head -c 12 <&3 | od -An -tx1' 2>&1 | tr -s ' ' | head -n1)"
fi
# The way UP: a node reaches its parent as any client does, over the public
# edge. UP_HOST is any hostname the parent's web head answers /healthz on.
line "HTTPS up, to $UP_HOST" "HTTP $(curl -s -m 10 -o /dev/null -w '%{http_code} in %{time_total}s' "https://$UP_HOST/healthz"), then $(curl -s -m 10 -o /dev/null -w '%{time_total}s' "https://$UP_HOST/healthz") again"
line "HTTPS to a raw address" "$(curl -s -m 5 -o /dev/null https://1.1.1.1/ 2>/dev/null && echo reached || echo refused)"
line "memory (total / available)" "$(awk '/MemTotal/{t=$2} /MemAvailable/{a=$2} END{printf "%d MB / %d MB", t/1024, a/1024}' /proc/meminfo)"
line "disk (/)" "$(df -h / | awk 'NR==2{print $2 " total, " $4 " free"}')"
# What the workspace does to a process while nobody calls it. A heartbeat
# writes the time once a second; the largest gap between two beats is how long
# the workspace was frozen. The first call starts it, later calls read it.
hb=/tmp/txco-node-heartbeat
if ! pgrep -f txco-node-heartbeat >/dev/null 2>&1; then
  setsid bash -c 'while :; do date +%s >> /tmp/txco-node-heartbeat; sleep 1; done' </dev/null >/dev/null 2>&1 &
  line "frozen while idle" "heartbeat started; call /facts again after an idle minute"
else
  line "frozen while idle" "$(awk 'NR>1 { g = $1 - p; if (g > m) m = g } { p = $1 } END { printf "%d beats, longest gap %d s", NR, m }' "$hb")"
fi
