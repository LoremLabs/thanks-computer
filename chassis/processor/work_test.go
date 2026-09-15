package processor

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerCountsAndIdles(t *testing.T) {
	tr := NewTracker()
	select {
	case <-tr.Idle():
	default:
		t.Fatal("a new tracker should be idle")
	}

	end1 := tr.Begin()
	end2 := tr.Begin()
	if got := tr.Count(); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	idle := tr.Idle()
	end1()
	end1() // idempotent
	select {
	case <-idle:
		t.Fatal("idle closed with work still in flight")
	default:
	}
	end2()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("idle not closed after the last end")
	}
	if got := tr.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}

	// Work that begins after a zero count gets a fresh idle channel.
	end3 := tr.Begin()
	select {
	case <-tr.Idle():
		t.Fatal("idle with work in flight")
	default:
	}
	end3()
}

// Begin racing a waiter at a zero count is the case sync.WaitGroup forbids.
func TestTrackerBeginWhileWaiting(t *testing.T) {
	tr := NewTracker()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			end := tr.Begin()
			end()
		}()
		go func() {
			defer wg.Done()
			select {
			case <-tr.Idle():
			case <-time.After(time.Second):
				t.Error("Idle never closed")
			}
		}()
	}
	wg.Wait()
	if got := tr.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
}

func TestTrackerNilSafe(t *testing.T) {
	var tr *Tracker
	tr.Begin()()
	if tr.Count() != 0 {
		t.Fatal("nil tracker counts nothing")
	}
	select {
	case <-tr.Idle():
	default:
		t.Fatal("nil tracker is always idle")
	}
}
