package outlettest

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// SOCKS5Server is a minimal SOCKS5 CONNECT relay for tests: no auth, IPv4,
// IPv6 and domain targets, records every CONNECT it was asked for. Serve it
// on any net.Listener — a loopback port, or a listener inside a tunnel.
type SOCKS5Server struct {
	mu      sync.Mutex
	targets []string
}

// Serve accepts on ln until it closes.
func (s *SOCKS5Server) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

// Targets returns every CONNECT target seen, in order.
func (s *SOCKS5Server) Targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.targets...)
}

func (s *SOCKS5Server) handle(c net.Conn) {
	defer c.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	if _, err := io.ReadFull(c, make([]byte, int(hdr[1]))); err != nil {
		return
	}
	_, _ = c.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		_, _ = io.ReadFull(c, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		b := make([]byte, int(l[0]))
		_, _ = io.ReadFull(c, b)
		host = string(b)
	default:
		return
	}
	pb := make([]byte, 2)
	_, _ = io.ReadFull(c, pb)
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))
	s.mu.Lock()
	s.targets = append(s.targets, target)
	s.mu.Unlock()
	up, err := net.DialTimeout("tcp", target, 2*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}
