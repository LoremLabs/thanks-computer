package attach

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestNetConnBytesOnlyAndWait: a service Conn passes bytes both ways,
// refuses every control, and Wait returns as soon as the far side closes —
// without anyone calling Close on the near side.
func TestNetConnBytesOnlyAndWait(t *testing.T) {
	near, far := net.Pipe()
	c := NewNet(near)

	go func() { _, _ = far.Write([]byte("rfb")) }()
	buf := make([]byte, 8)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "rfb" {
		t.Fatalf("read = %q err=%v", buf[:n], err)
	}
	go func() {
		b := make([]byte, 8)
		k, _ := far.Read(b)
		if string(b[:k]) != "key" {
			t.Errorf("far read = %q", b[:k])
		}
	}()
	if _, err := c.Write([]byte("key")); err != nil {
		t.Fatal(err)
	}
	if err := c.Control(context.Background(), ControlResize, []byte(`{"cols":1,"rows":1}`)); err == nil {
		t.Error("resize accepted on a service connection")
	}

	waited := make(chan int, 1)
	go func() { exit, _ := c.Wait(); waited <- exit }()
	select {
	case <-waited:
		t.Fatal("Wait returned before the connection ended")
	case <-time.After(20 * time.Millisecond):
	}
	_ = far.Close()
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("read after far close = %v, want EOF", err)
	}
	select {
	case exit := <-waited:
		if exit != 0 {
			t.Errorf("exit = %d", exit)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after EOF")
	}
	if err := c.Close(); err == nil {
		// net.Pipe reports closed on a second close; either way it is idempotent.
	}
	_ = c.Close()
}

// TestRegistryServiceKind: a service binding is registered, looked up and
// listed exactly like a PTY one, and carries its service name.
func TestRegistryServiceKind(t *testing.T) {
	reg := NewRegistry()
	near, far := net.Pipe()
	defer far.Close()
	att := New(context.Background(), Params{
		ID: "l1", Tenant: "acme", AppStack: "app", Workspace: "tools", Kind: KindService, Service: "browser",
		SessionID: "s1", NodeID: "n", StartedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	att.Conn = NewNet(near)
	if err := reg.Bind(att); err != nil {
		t.Fatal(err)
	}
	got, ok := reg.Lookup("acme", "s1")
	if !ok || got.Kind != KindService || got.Service != "browser" {
		t.Fatalf("lookup = %+v ok=%v", got, ok)
	}
	if err := reg.Bind(att); err != ErrAlreadyBound {
		t.Errorf("second bind = %v", err)
	}
	att.Close("test")
	if _, ok := reg.Lookup("acme", "s1"); ok {
		t.Error("closed attachment still looked up")
	}
	if att.Reason() != "test" {
		t.Errorf("reason = %q", att.Reason())
	}
}
