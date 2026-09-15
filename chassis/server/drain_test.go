package server

import (
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

func TestRefusedWhileDraining(t *testing.T) {
	for src, want := range map[string]bool{
		"http": true, "lmtp": true, "tcp": true,
		// Nobody retries these, so a draining node runs them.
		"scheduled": false, "cron": false, "source": false, "imap": false,
		"dns": false, "calendar": false, "contacts": false, "": false,
	} {
		if got := refusedWhileDraining(src); got != want {
			t.Errorf("refusedWhileDraining(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestDrainBeforeStopZeroGraceSkips(t *testing.T) {
	t.Cleanup(func() { admission.SetDraining(false) })
	tr := processor.NewTracker()
	defer tr.Begin()() // busy, but a zero grace must not wait for it
	start := time.Now()
	drainBeforeStop(zap.NewNop(), "0", tr)
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("grace 0 waited")
	}
	if admission.IsDraining() {
		t.Fatal("grace 0 must keep the old shutdown: no drain")
	}
}

func TestDrainBeforeStopIdleReturnsAtOnce(t *testing.T) {
	t.Cleanup(func() { admission.SetDraining(false) })
	start := time.Now()
	drainBeforeStop(zap.NewNop(), "5s", processor.NewTracker(), nil)
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("an idle node waited")
	}
	if !admission.IsDraining() {
		t.Fatal("drain must be on so nothing new starts")
	}
}

func TestDrainBeforeStopWaitsForWork(t *testing.T) {
	t.Cleanup(func() { admission.SetDraining(false) })
	requests, detached := processor.NewTracker(), processor.NewTracker()
	endReq := requests.Begin()
	endDetached := detached.Begin()
	go func() {
		time.Sleep(60 * time.Millisecond)
		endReq()
		// A request that hands off to detached work: the drain must wait
		// for that too, not return when the request goroutine ends.
		time.Sleep(90 * time.Millisecond)
		endDetached()
	}()
	start := time.Now()
	drainBeforeStop(zap.NewNop(), "5s", requests, detached)
	if el := time.Since(start); el < 140*time.Millisecond || el > 2*time.Second {
		t.Fatalf("drain returned after %v, want once both trackers were idle (~150ms)", el)
	}
}

func TestDrainBeforeStopHonorsGrace(t *testing.T) {
	t.Cleanup(func() { admission.SetDraining(false) })
	tr := processor.NewTracker()
	defer tr.Begin()() // never ends within the grace
	start := time.Now()
	drainBeforeStop(zap.NewNop(), "120ms", tr)
	if el := time.Since(start); el < 120*time.Millisecond || el > time.Second {
		t.Fatalf("drain returned after %v, want the 120ms grace", el)
	}
}
