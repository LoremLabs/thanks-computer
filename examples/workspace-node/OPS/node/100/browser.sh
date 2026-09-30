set -eu
[ "$(uname -s)" = Linux ] || { echo '{"ready":false,"error":"needs a Linux workspace (a real provider); the local provider would install onto your own machine"}'; exit 0; }
MARK=/var/lib/workspace-runtime/version
LOG=/tmp/ws-setup.log
CHROME="$(command -v google-chrome-stable || command -v google-chrome || command -v chromium || command -v chromium-browser || true)"
vncup() { ss -ltn 2>/dev/null | grep -q ':5900 '; }
# Chrome's debugging port is what /ui/open drives. Chrome takes a few seconds
# to open it after it starts, so "ready" waits for it as well as the display.
cdpup() { curl -fsS -m 2 http://127.0.0.1:9222/json/version >/dev/null 2>&1; }
if [ "$(cat "$MARK" 2>/dev/null || echo 0)" = "$REQ" ] && [ -n "$CHROME" ] && vncup && cdpup; then
  echo "{\"ready\":true,\"chromium\":\"$CHROME\",\"vnc\":\"yes\"}"; exit 0
fi
if pgrep -f ws-provision.sh >/dev/null 2>&1; then
  ph="$(tail -n1 "$LOG" 2>/dev/null | tr -d '"\\' | tr '\n' ' ')"
  echo "{\"ready\":false,\"phase\":\"${ph:-working}\"}"; exit 0
fi
cat > /tmp/ws-provision.sh <<'WORKEOF'
set -eu
MARK=/var/lib/workspace-runtime/version
if [ "$(cat "$MARK" 2>/dev/null || echo 0)" != "$REQ" ]; then
  echo "PHASE provision req=$REQ"
  export DEBIAN_FRONTEND=noninteractive
  ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
  if [ "$ARCH" = amd64 ]; then
    # Ubuntu's `chromium` apt package is a snap stub that won't run in the VM;
    # Google Chrome ships a real .deb. Armored key referenced directly (no gnupg).
    sudo install -d -m0755 /etc/apt/keyrings
    curl -fsSL https://dl.google.com/linux/linux_signing_key.pub | sudo tee /etc/apt/keyrings/google-chrome.asc >/dev/null
    echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google-chrome.asc] http://dl.google.com/linux/chrome/deb/ stable main" | sudo tee /etc/apt/sources.list.d/google-chrome.list >/dev/null
    BROWSER=google-chrome-stable
  else
    BROWSER=chromium
  fi
  sudo timeout 150 apt-get update -o DPkg::Lock::Timeout=120
  sudo timeout 480 apt-get install -y -o DPkg::Lock::Timeout=120 --no-install-recommends "$BROWSER" xvfb x11vnc fonts-liberation openbox
  sudo mkdir -p "$(dirname "$MARK")"; printf '%s\n' "$REQ" | sudo tee "$MARK" >/dev/null
  # New runtime version: retire the old daemons so they relaunch with new flags.
  pkill -x openbox 2>/dev/null || true; pkill -x x11vnc 2>/dev/null || true; pkill -x Xvfb 2>/dev/null || true; pkill -f "user-data-dir=$HOME/browser-profile" 2>/dev/null || true
  # Wait for them to be gone. The launch below starts only what is not
  # running, and a daemon that is still exiting would be counted as running.
  for i in $(seq 1 50); do
    pgrep -x Xvfb >/dev/null || pgrep -x x11vnc >/dev/null || pgrep -x openbox >/dev/null || pgrep -f "user-data-dir=$HOME/browser-profile" >/dev/null || break
    sleep 0.2
  done
  echo "PHASE provisioned"
fi
CHROME="$(command -v google-chrome-stable || command -v google-chrome || command -v chromium || command -v chromium-browser || true)"
echo "PHASE launch chrome=${CHROME:-none}"
[ -n "$CHROME" ] || { echo "PHASE error no-chromium"; exit 1; }
sudo mkdir -p /tmp/.X11-unix; sudo chown root:root /tmp/.X11-unix 2>/dev/null || true; sudo chmod 1777 /tmp/.X11-unix
export DISPLAY=:99
if ! pgrep -x Xvfb >/dev/null; then sudo rm -f /tmp/.X99-lock /tmp/.X11-unix/X99 2>/dev/null || true; setsid Xvfb :99 -screen 0 1280x800x24 -nolisten tcp </dev/null >/tmp/ws-xvfb.log 2>&1 & fi
for i in $(seq 1 25); do [ -e /tmp/.X11-unix/X99 ] && break; sleep 0.2; done
if ! pgrep -x openbox >/dev/null; then setsid openbox </dev/null >/tmp/ws-openbox.log 2>&1 & sleep 1; fi
# Chrome runs WITH its sandbox: the workspace allows unprivileged user
# namespaces, so --no-sandbox is not needed (and Chrome shows a warning bar
# under it). It gets a session bus of its own where dbus is installed, which
# is what it looks for when it starts; without one it only fills its log with
# "Failed to connect to the bus". The profile is kept, so a sign-in to the
# node's admin UI survives a cold start. The debugging port is how /ui/open
# points the browser at a page; it listens on loopback only.
BUS=""; command -v dbus-run-session >/dev/null 2>&1 && BUS="dbus-run-session --"
if ! pgrep -f "user-data-dir=$HOME/browser-profile" >/dev/null; then setsid $BUS "$CHROME" --disable-gpu --disable-dev-shm-usage --password-store=basic --user-data-dir="$HOME/browser-profile" --no-first-run --no-default-browser-check --remote-debugging-port=9222 --remote-allow-origins=* --window-position=0,0 --window-size=1280,800 about:blank </dev/null >/tmp/ws-chromium.log 2>&1 & fi
# x11vnc: NOT -localhost — the connect tunnel arrives from the sprite gateway,
# not loopback. Allow loopback + the default gateway (the tunnel's source);
# the port is reachable only through the authorized chassis tunnel anyway.
GW="$(ip route 2>/dev/null | awk '/^default/{print $3; exit}')"
ALLOW=""; [ -n "$GW" ] && ALLOW="-allow 127.0.0.1,$GW"
if ! pgrep -x x11vnc >/dev/null; then setsid x11vnc -display :99 -rfbport 5900 -forever -shared -nopw -noxdamage -nolookup $ALLOW </dev/null >/tmp/ws-x11vnc.log 2>&1 & fi
echo "PHASE waiting for the display and the browser"
for i in $(seq 1 60); do ss -ltn 2>/dev/null | grep -q ':5900 ' && break; sleep 0.5; done
for i in $(seq 1 60); do curl -fsS -m 2 http://127.0.0.1:9222/json/version >/dev/null 2>&1 && break; sleep 0.5; done
echo "PHASE ready gw=${GW:-none}"
WORKEOF
setsid bash /tmp/ws-provision.sh </dev/null >"$LOG" 2>&1 &
echo "{\"ready\":false,\"phase\":\"starting\"}"