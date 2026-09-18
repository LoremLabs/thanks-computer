package tcp

import (
	"fmt"
	"strings"
)

// listenerSpec is one parsed --tcp-listen-addrs entry:
//
//	name=addr[;tls][;self-signed]
//
// Options follow the address, ';'-separated (',' is the flag's own list
// separator, so it can never appear inside an entry). `tls` terminates
// TLS on the listener with the bundled cert manager and makes the SNI
// hostname the connection's routing fact (@tcp.host); `self-signed`
// serves the dev certificate instead and implies tls.
type listenerSpec struct {
	Name       string
	Addr       string
	TLS        bool
	SelfSigned bool
}

// parseTCPListenSpec splits one `--tcp-listen-addrs` entry into its
// operator-chosen name and the address to bind. Form `name=addr`
// picks the name; bare `addr` falls back to `"default"` so existing
// configs keep stamping `_txc.tcp.listener = "default"` (and any
// ingress YAML keyed on it keeps matching). Empty input returns
// ("", "") so callers can drop blank entries (viper's CSV parsing
// occasionally produces them).
func parseTCPListenSpec(spec string) (name, addr string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", ""
	}
	if i := strings.Index(spec, "="); i >= 0 {
		name = strings.TrimSpace(spec[:i])
		addr = strings.TrimSpace(spec[i+1:])
		if name == "" {
			name = "default"
		}
		return name, addr
	}
	return "default", spec
}

// parseListenerSpec parses one entry with its options. A blank entry
// yields an empty Addr (the caller skips it); an unknown option is an
// error — a typo must not silently bind a plaintext listener where the
// operator meant TLS.
func parseListenerSpec(spec string) (listenerSpec, error) {
	parts := strings.Split(spec, ";")
	name, addr := parseTCPListenSpec(parts[0])
	ls := listenerSpec{Name: name, Addr: addr}
	if addr == "" {
		return ls, nil
	}
	for _, opt := range parts[1:] {
		switch opt = strings.TrimSpace(opt); opt {
		case "":
		case "tls":
			ls.TLS = true
		case "self-signed":
			ls.TLS, ls.SelfSigned = true, true
		default:
			return ls, fmt.Errorf("tcp listener %q: unknown option %q (want tls, self-signed)", spec, opt)
		}
	}
	return ls, nil
}

// parseListenerSpecs parses every entry, dropping blanks.
func parseListenerSpecs(entries []string) ([]listenerSpec, error) {
	var out []listenerSpec
	for _, e := range entries {
		ls, err := parseListenerSpec(e)
		if err != nil {
			return nil, err
		}
		if ls.Addr == "" {
			continue
		}
		out = append(out, ls)
	}
	return out, nil
}
