package allowance

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/usage"
)

// FlushInterval is how often the Tee writes accumulated charges to the KV.
// It bounds how stale an allowance's counter is for the next request's check.
const FlushInterval = time.Second

// Tee is a usage.Sink in front of the configured one. Every event goes on to
// the next sink unchanged; a billable event that ran under an allowance also
// adds its fuel to that allowance's counter. Charges are summed in memory and
// written once per FlushInterval per allowance, so a busy allowance costs one
// KV increment a second, not one per request.
type Tee struct {
	next  usage.Sink
	store *Store
	log   *zap.Logger

	mu      sync.Mutex
	pending map[chargeKey]int64

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

type chargeKey struct{ tenant, name string }

// NewTee wraps next (which may be nil) and starts the flush loop.
func NewTee(next usage.Sink, store *Store, logger *zap.Logger) *Tee {
	if logger == nil {
		logger = zap.NewNop()
	}
	t := &Tee{
		next: next, store: store, log: logger,
		pending: map[chargeKey]int64{},
		stop:    make(chan struct{}), done: make(chan struct{}),
	}
	go t.loop()
	return t
}

// WriteEvent forwards ev and, when it is billable fuel spent under an
// allowance, adds it to the pending charges.
func (t *Tee) WriteEvent(ev usage.UsageEvent) {
	if t.next != nil {
		t.next.WriteEvent(ev)
	}
	if ev.Allowance == "" || !ev.Billable || ev.Fuel <= 0 || ev.Tenant == "" || !ValidName(ev.Allowance) {
		return
	}
	k := chargeKey{tenant: ev.Tenant, name: ev.Allowance}
	t.mu.Lock()
	t.pending[k] += ev.Fuel
	t.mu.Unlock()
}

// Name reports the wrapped sink's name: the tee is not a backend of its own.
func (t *Tee) Name() string {
	if t.next != nil {
		return t.next.Name()
	}
	return "allowance"
}

// Close stops the loop, writes what is pending within ctx, then closes the
// wrapped sink.
func (t *Tee) Close(ctx context.Context) error {
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
	t.flush(ctx)
	if t.next != nil {
		return t.next.Close(ctx)
	}
	return nil
}

func (t *Tee) loop() {
	defer close(t.done)
	tick := time.NewTicker(FlushInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*FlushInterval)
			t.flush(ctx)
			cancel()
		}
	}
}

// flush swaps the pending map out and writes each charge. A failed write
// (the KV unreachable, increment contention) is put back for the next tick,
// so a charge is late rather than lost.
func (t *Tee) flush(ctx context.Context) {
	t.mu.Lock()
	batch := t.pending
	t.pending = map[chargeKey]int64{}
	t.mu.Unlock()
	for k, fuel := range batch {
		if err := t.store.Charge(ctx, k.tenant, k.name, fuel); err != nil {
			t.log.Warn("allowance: charge failed; retrying next flush",
				zap.String("tenant", k.tenant), zap.String("allowance", k.name),
				zap.Int64("fuel", fuel), zap.Error(err))
			t.mu.Lock()
			t.pending[k] += fuel
			t.mu.Unlock()
		}
	}
}
