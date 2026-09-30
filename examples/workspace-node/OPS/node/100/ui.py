# The node's browser, driven over Chrome's debugging port on the node's own
# loopback.
#   MODE=open  sign the browser in to the node's admin UI
#   MODE=shot  print a PNG of what the browser shows, base64
# Standard library only: nothing to install on the workspace.
import base64, json, os, re, socket, struct, subprocess, sys, time, urllib.request

CDP = "http://127.0.0.1:9222"


def page_ws():
    # A browser that has just started may not be listening yet, or may not
    # have opened its first page: give it a few seconds before giving up.
    why = "no page open"
    for _ in range(20):
        try:
            tabs = json.load(urllib.request.urlopen(CDP + "/json", timeout=5))
            pages = [t for t in tabs if t.get("type") == "page" and t.get("webSocketDebuggerUrl")]
            if pages:
                return pages[0]["webSocketDebuggerUrl"]
        except Exception as e:
            why = repr(e)
        time.sleep(0.5)
    sys.exit("the node's browser is not running (GET /ui/setup first): " + why)


class WS:
    """A WebSocket client just big enough for Chrome's debugging protocol."""

    def __init__(self, url):
        hostport, _, path = url[5:].partition("/")  # ws://host:port/path
        host, port = hostport.split(":")
        self.s = socket.create_connection((host, int(port)), timeout=30)
        key = base64.b64encode(os.urandom(16)).decode()
        self.s.sendall(("GET /%s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"
                        "Connection: Upgrade\r\nSec-WebSocket-Key: %s\r\n"
                        "Sec-WebSocket-Version: 13\r\n\r\n" % (path, hostport, key)).encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            c = self.s.recv(1)
            if not c:
                sys.exit("the browser closed the connection during the handshake")
            buf += c
        line0 = buf.split(b"\r\n", 1)[0].decode("latin1")
        if " 101 " not in line0:
            sys.exit("the browser refused the connection: " + line0)
        self.n = 0

    def _read(self, n):
        b = bytearray()
        while len(b) < n:
            c = self.s.recv(min(65536, n - len(b)))
            if not c:
                sys.exit("the browser closed the connection")
            b += c
        return bytes(b)

    def send(self, obj):
        p = json.dumps(obj).encode()
        n, m, f = len(p), os.urandom(4), bytearray([0x81])
        if n < 126:
            f.append(0x80 | n)
        elif n < 65536:
            f.append(0x80 | 126)
            f += struct.pack(">H", n)
        else:
            f.append(0x80 | 127)
            f += struct.pack(">Q", n)
        f += m
        f += bytes(b ^ m[i % 4] for i, b in enumerate(p))
        self.s.sendall(f)

    def recv(self):
        data = b""
        while True:
            h = self._read(2)
            fin, op, n = h[0] & 0x80, h[0] & 0x0F, h[1] & 0x7F
            if n == 126:
                n = struct.unpack(">H", self._read(2))[0]
            elif n == 127:
                n = struct.unpack(">Q", self._read(8))[0]
            p = self._read(n) if n else b""
            if op == 0x8:
                sys.exit("the browser closed the connection")
            if op in (0x9, 0xA):  # ping, pong
                continue
            data += p
            if fin:
                return json.loads(data)

    def call(self, method, params=None):
        self.n += 1
        self.send({"id": self.n, "method": method, "params": params or {}})
        while True:
            m = self.recv()
            if m.get("id") == self.n:
                if "error" in m:
                    sys.exit("%s: %s" % (method, m["error"].get("message")))
                return m.get("result", {})


mode = os.environ.get("MODE", "")
if mode == "open":
    # `txco ui` asks the node's admin plane for a one-time sign-in link, signing
    # with the node-local key. The link goes to the browser and nowhere else:
    # it is never printed.
    admin = "http://127.0.0.1:" + os.environ["ADMIN_PORT"]
    out = subprocess.run(
        ["/opt/txco/bin/txco", "ui", "--profile", "node", "--no-open", "--url", admin, "--label", "workspace-node"],
        capture_output=True, text=True, env=dict(os.environ, TXCO_HOME="/var/lib/txco/home"))
    # The output names the chassis too; the sign-in link is the URL with a token.
    link = re.search(r"https?://\S+#login\?t=\S+", out.stdout)
    if not link:
        sys.exit("txco ui gave no sign-in link (exit %d): %s" % (out.returncode, out.stderr.strip()[-300:]))
    ws = WS(page_ws())
    ws.call("Page.navigate", {"url": link.group(0)})
    time.sleep(2)  # the page trades the link for a session cookie
    # Then the view asked for: a route of the admin UI, such as "traces".
    goto = os.environ.get("GOTO", "")
    if re.fullmatch(r"[a-z]+(/[A-Za-z0-9_-]+)?", goto):
        ws.call("Page.navigate", {"url": admin + "/admin/#" + goto})
    print("the node's browser is signed in to the node's admin UI at %s/admin/" % admin)
elif mode == "shot":
    ws = WS(page_ws())
    sys.stdout.write(ws.call("Page.captureScreenshot", {"format": "png"})["data"])
else:
    sys.exit("MODE must be open or shot")
