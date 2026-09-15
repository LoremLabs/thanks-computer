package server

import (
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// drainPollInterval is how often drainBeforeStop re-checks the trackers.
const drainPollInterval = 50 * time.Millisecond

// refusedWhileDraining reports whether a draining node answers a new envelope
// from this inlet with the drain denial instead of running it. Only peers that
// come back on their own are refused: a web client or load balancer goes
// elsewhere on 503 + Retry-After, an MTA requeues on LMTP's 451, a TCP client
// reconnects. Everything else has nobody to retry it — the scheduled inlet,
// for one, marks its row done on any answer — so it runs, and the pollers
// stop claiming new rows while draining instead.
func refusedWhileDraining(src string) bool {
	switch src {
	case "http", "lmtp", "tcp":
		return true
	}
	return false
}

// drainBeforeStop lets in-flight work finish before shutdown cancels the
// context it runs on. It turns the node's drain on — new web requests get 503,
// new LMTP deliveries 451, the schedulers claim nothing new — then waits, up
// to grace, until no tracked work is in flight: the bus loop's requests and
// the processor's detached work (a continuation's tail, a local async op, a
// worker callback's resume). A grace of 0 or an unparsable one skips it all:
// the old cancel-at-once shutdown. Work still running at the deadline is
// cancelled by the caller, as before.
func drainBeforeStop(logger *zap.Logger, grace string, trackers ...*processor.Tracker) {
	d, err := time.ParseDuration(grace)
	if err != nil || d <= 0 {
		return
	}
	admission.SetDraining(true)
	busy := func() int {
		n := 0
		for _, t := range trackers {
			n += t.Count()
		}
		return n
	}
	if busy() == 0 {
		return
	}
	logger.Info("draining in-flight work before shutdown",
		zap.Int("in_flight", busy()), zap.Duration("grace", d))
	deadline := time.Now().Add(d)
	tick := time.NewTicker(drainPollInterval)
	defer tick.Stop()
	for range tick.C {
		n := busy()
		if n == 0 {
			logger.Info("drained")
			return
		}
		if time.Now().After(deadline) {
			logger.Warn("shutdown grace expired; cancelling in-flight work",
				zap.Int("in_flight", n), zap.Duration("grace", d))
			return
		}
	}
}
