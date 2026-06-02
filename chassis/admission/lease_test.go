package admission

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestLeaseRunsReleasesOnce(t *testing.T) {
	l := NewLease()
	var n atomic.Int64
	l.onRelease(func() { n.Add(1) })
	l.onRelease(func() { n.Add(1) })
	l.Release()
	l.Release() // idempotent — must not run them again
	if n.Load() != 2 {
		t.Errorf("release fns ran %d times, want 2 (once each)", n.Load())
	}
}

func TestLeaseNilSafe(t *testing.T) {
	var l *Lease
	l.onRelease(func() { t.Fatal("nil lease should not register") })
	l.Release() // must not panic
}

func TestLeaseContextRoundTrip(t *testing.T) {
	l := NewLease()
	ctx := WithLease(context.Background(), l)
	if LeaseFromContext(ctx) != l {
		t.Error("lease not round-tripped through context")
	}
	if LeaseFromContext(context.Background()) != nil {
		t.Error("missing lease should read as nil")
	}
}
