package workspace

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Service is one named endpoint on a workspace's OWN loopback that a stack
// may bind to a WebSocket session with `workspace://<name>/connect WITH
// service = "<name>"`. The name is the whole vocabulary a stack has: no
// host, port or scheme is reachable from a WITH value, so envelope data —
// including a model's output that reached one — can pick a service the
// chassis knows about and nothing else. That is what keeps connect an
// attachment rather than a tunnel.
type Service struct {
	Name        string
	Port        int // on the workspace's loopback; the provider dials inside the workspace
	Description string
}

// Addr is the loopback address the provider dials for the service.
func (s Service) Addr() string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)) }

// Dialer is the optional capability behind the connect verb: a woken
// workspace that hands back a byte stream to one of its own local
// services. As with Starter, a provider lists "connect" in Capabilities
// for the boot log, but the gate is this interface assertion.
type Dialer interface {
	DialService(ctx context.Context, svc Service) (net.Conn, error)
}

// builtinServices is the table the chassis ships. An operator may ADD
// names with --workspace-services (name=port); a built-in is never
// overridden, so `browser` means the same thing on every node.
var builtinServices = map[string]Service{
	"browser": {Name: "browser", Port: 5900, Description: "the workspace's VNC display (x11vnc on the browser's X server)"},
}

// serviceNameRE bounds an operator-added service name: short, lower-case,
// DNS-label-shaped, so it reads cleanly in a WITH clause and an error.
var serviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Services is the closed name → Service table the connect verb resolves
// against.
type Services struct {
	byName map[string]Service
}

// NewServices builds the table: the built-ins plus each operator entry
// ("name=port"). A malformed entry, a port outside 1–65535, a duplicate, or
// an attempt to redefine a built-in is an error — configuration, not data,
// so the caller decides whether that is fatal.
func NewServices(extra []string) (*Services, error) {
	s := &Services{byName: make(map[string]Service, len(builtinServices)+len(extra))}
	for k, v := range builtinServices {
		s.byName[k] = v
	}
	for _, raw := range extra {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name, port, err := ParseServiceEntry(raw)
		if err != nil {
			return nil, err
		}
		if _, builtin := builtinServices[name]; builtin {
			return nil, fmt.Errorf("workspace: service %q is built in and cannot be redefined", name)
		}
		if _, dup := s.byName[name]; dup {
			return nil, fmt.Errorf("workspace: service %q listed twice", name)
		}
		s.byName[name] = Service{Name: name, Port: port, Description: "operator-declared (--workspace-services)"}
	}
	return s, nil
}

// ParseServiceEntry parses one "name=port" operator entry.
func ParseServiceEntry(raw string) (name string, port int, err error) {
	name, portStr, ok := strings.Cut(raw, "=")
	name = strings.TrimSpace(name)
	portStr = strings.TrimSpace(portStr)
	if !ok || name == "" || portStr == "" {
		return "", 0, fmt.Errorf("workspace: service entry %q: want name=port", raw)
	}
	if !serviceNameRE.MatchString(name) {
		return "", 0, fmt.Errorf("workspace: service entry %q: name must match %s", raw, serviceNameRE)
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("workspace: service entry %q: port must be 1–65535", raw)
	}
	return name, port, nil
}

// DefaultServices is the built-in table alone.
func DefaultServices() *Services {
	s, _ := NewServices(nil)
	return s
}

// Lookup resolves a service by name.
func (s *Services) Lookup(name string) (Service, bool) {
	if s == nil {
		return Service{}, false
	}
	svc, ok := s.byName[name]
	return svc, ok
}

// Names lists the known services, sorted, for error messages and the boot log.
func (s *Services) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.byName))
	for k := range s.byName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SetServices installs the table the connect verb resolves against; the
// built-ins alone until it is called.
func (m *Manager) SetServices(s *Services) {
	m.mu.Lock()
	m.services = s
	m.mu.Unlock()
}

// Services returns the table (never nil).
func (m *Manager) Services() *Services {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.services == nil {
		m.services = DefaultServices()
	}
	return m.services
}

// Dial wakes the workspace and opens a byte stream to one of its local
// services through the provider's Dialer. The ctx bounds the connection the
// way Start's bounds a session: when it ends, the connection is closed. The
// run id is minted or reused exactly as for an exec, and the identity table
// is touched, so a live connection counts as use. A provider without the
// Dialer capability reports "unsupported"; a workspace gone at the provider
// heals as Exec does.
func (m *Manager) Dial(ctx context.Context, spec Spec, svc Service) (net.Conn, Handle, string, error) {
	conn, h, runID, err := m.dialOnce(ctx, spec, svc)
	if err != nil && IsNotFound(err) {
		m.dropIdentity(ctx, spec)
		conn, h, runID, err = m.dialOnce(ctx, spec, svc)
	}
	return conn, h, runID, err
}

func (m *Manager) dialOnce(ctx context.Context, spec Spec, svc Service) (net.Conn, Handle, string, error) {
	if svc.Name == "" || svc.Port <= 0 {
		return nil, Handle{}, "", &Error{Code: "bad_request", Message: "connect: no service"}
	}
	e, err := m.lookup(ctx, spec)
	if err != nil {
		return nil, Handle{}, "", err
	}
	comp, err := m.prov.Wake(ctx, e.h)
	if err != nil {
		m.forget(spec)
		return nil, e.h, "", err
	}
	d, ok := comp.(Dialer)
	if !ok {
		return nil, e.h, "", &Error{Code: "unsupported", Message: "provider " + m.prov.Name() + " has no connect capability"}
	}
	runID := m.runIDFor(e, comp)
	conn, err := d.DialService(ctx, svc)
	now := m.now()
	e.runID, e.lastUsed = runID, now
	m.remember(identityKey(spec), e)
	if m.store != nil {
		_ = m.store.Touch(ctx, ID(spec.Tenant, spec.Stack, spec.Name), now, runID, StatusRunning)
	}
	if err != nil {
		return nil, e.h, runID, err
	}
	if conn == nil {
		return nil, e.h, runID, errors.New("workspace: provider returned no connection")
	}
	return conn, e.h, runID, nil
}
