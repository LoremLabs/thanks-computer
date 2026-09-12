package workspace

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestServicesBuiltinsAndOperatorEntries(t *testing.T) {
	s := DefaultServices()
	b, ok := s.Lookup("browser")
	if !ok || b.Port != 5900 || b.Addr() != "127.0.0.1:5900" {
		t.Fatalf("browser = %+v ok=%v", b, ok)
	}
	if _, ok := s.Lookup("echo"); ok {
		t.Fatal("echo should not exist by default")
	}

	s, err := NewServices([]string{"echo=17777", " vnc-2 = 5901 ", ""})
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Lookup("echo"); !ok || e.Port != 17777 || e.Addr() != "127.0.0.1:17777" {
		t.Errorf("echo = %+v ok=%v", e, ok)
	}
	if got := strings.Join(s.Names(), ","); got != "browser,echo,vnc-2" {
		t.Errorf("names = %q", got)
	}
	var nilS *Services
	if _, ok := nilS.Lookup("browser"); ok || nilS.Names() != nil {
		t.Error("nil table must answer nothing")
	}
}

func TestServicesRejectsBadEntries(t *testing.T) {
	for _, bad := range [][]string{
		{"browser=1"},        // a built-in is never redefined
		{"x"},                // no port
		{"=5"},               // no name
		{"echo=0"},           // port range
		{"echo=70000"},       // port range
		{"echo=abc"},         // not a number
		{"Echo=1"},           // name grammar
		{"a b=1"},            // name grammar
		{"echo=1", "echo=2"}, // duplicate
	} {
		if _, err := NewServices(bad); err == nil {
			t.Errorf("NewServices(%q) accepted", bad)
		}
	}
}

// dialableFake is a fakeProvider whose woken computer also implements
// Dialer: DialService hands back one end of a pipe and keeps the other.
type dialableFake struct {
	*fakeProvider
	dialErr error
	far     net.Conn
	seen    Service
	dials   int
}

func (d *dialableFake) Wake(ctx context.Context, h Handle) (Computer, error) {
	if _, err := d.fakeProvider.Wake(ctx, h); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *dialableFake) DialService(_ context.Context, svc Service) (net.Conn, error) {
	d.dials++
	d.seen = svc
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	near, far := net.Pipe()
	d.far = far
	return near, nil
}

func TestManagerDialRequiresDialer(t *testing.T) {
	m := NewManager(&fakeProvider{}, Limits{}, nil)
	svc, _ := m.Services().Lookup("browser")
	_, _, _, err := m.Dial(context.Background(), Spec{Tenant: "acme", Stack: "agents", Name: "tools"}, svc)
	var we *Error
	if !errors.As(err, &we) || we.Code != "unsupported" {
		t.Fatalf("err = %v, want unsupported", err)
	}
	if _, _, _, err := m.Dial(context.Background(), Spec{Tenant: "acme", Stack: "agents", Name: "tools"}, Service{}); err == nil {
		t.Fatal("an empty service must be refused")
	}
}

func TestManagerDialRoundTripsAndHeals(t *testing.T) {
	d := &dialableFake{fakeProvider: &fakeProvider{vanishOnce: true}}
	m := NewManager(d, Limits{}, nil)
	svcs, _ := NewServices([]string{"echo=17777"})
	m.SetServices(svcs)
	svc, _ := m.Services().Lookup("echo")
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}

	conn, h, run, err := m.Dial(context.Background(), spec, svc)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if run == "" || h.Ref != "ref/tools" || d.seen.Port != 17777 {
		t.Fatalf("run=%q h=%+v seen=%+v", run, h, d.seen)
	}
	// The first Wake reported the workspace gone: Dial healed it (a second
	// Create) rather than failing.
	if d.created != 2 || d.dials != 1 {
		t.Errorf("created=%d dials=%d, want 2 and 1 (heal then dial)", d.created, d.dials)
	}
	go func() { _, _ = d.far.Write([]byte("pong")) }()
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil || string(buf) != "pong" {
		t.Fatalf("read = %q err=%v", buf, err)
	}

	d.dialErr = &Error{Code: "unavailable", Message: "nothing listening"}
	if _, _, _, err := m.Dial(context.Background(), spec, svc); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("dial error not propagated: %v", err)
	}
}
