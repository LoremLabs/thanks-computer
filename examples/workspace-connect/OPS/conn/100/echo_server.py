# A TCP echo server on the workspace's loopback, left running after the exec
# that starts it (OPS/conn/100/start.txcl runs this with `python3 -c`).
import os
import socket
import socketserver
import sys

PORT = 17777

# Something already listens there: nothing to do, so /start is safe to repeat.
probe = socket.socket()
if probe.connect_ex(("127.0.0.1", PORT)) == 0:
    sys.exit(0)
probe.close()


class Echo(socketserver.BaseRequestHandler):
    def handle(self):
        for data in iter(lambda: self.request.recv(65536), b""):
            self.request.sendall(data)


socketserver.TCPServer.allow_reuse_address = True
srv = socketserver.TCPServer(("127.0.0.1", PORT), Echo)

# Double-fork into its own session so it outlives the exec (the local
# provider kills the exec's process group when the command returns).
if os.fork():
    sys.exit(0)
os.setsid()
if os.fork():
    sys.exit(0)
devnull = os.open(os.devnull, os.O_RDWR)
for fd in (0, 1, 2):
    os.dup2(devnull, fd)

# Exit after ten idle minutes.
srv.timeout = 600
srv.handle_timeout = lambda: sys.exit(0)
while True:
    srv.handle_request()
